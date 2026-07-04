package dashboard

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"probe/internal/proto"
)

// historyWindow is how far back we keep per-agent samples (for charts).
const historyWindow = 2 * time.Hour

// Store keeps a rolling window of state history per agent in SQLite plus a
// hot in-memory copy of each agent's latest state for fast reads.
type Store struct {
	mu        sync.RWMutex
	latest    map[string]*proto.State // agentID -> most recent state
	known     map[string]string       // agentID -> name
	deleted   map[string]struct{}     // admin-deleted agentIDs
	overrides map[string]string       // agentID -> override name (cached)
	db        *sql.DB
}

// NewStore opens (or creates) the SQLite database and ensures tables exist.
func NewStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // sqlite serializes writes anyway
	// 启用 WAL 模式提高并发读写性能,设 busy_timeout 防止锁冲突
	_, _ = db.Exec(`PRAGMA journal_mode=WAL`)
	_, _ = db.Exec(`PRAGMA busy_timeout=5000`)
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	// Auto-migrate: add columns that may not exist in older databases.
	for _, col := range []struct{ tbl, col, typ string }{
		{"servers", "override_name", "TEXT"},
		{"servers", "sort_order", "INTEGER DEFAULT 0"},
	} {
		db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, col.tbl, col.col, col.typ))
	}
	// Backfill sort_order for rows that predate the column (all 0).
	// Assign sequential values in name order so the initial display is
	// stable and matches the previous name-sorted behaviour.
	var needBackfill int
	_ = db.QueryRow(`SELECT COUNT(*) FROM servers WHERE sort_order = 0`).Scan(&needBackfill)
	if needBackfill > 0 {
		brows, _ := db.Query(`SELECT id FROM servers ORDER BY COALESCE(override_name,name), first_seen`)
		var bids []string
		for brows.Next() {
			var bid string
			if brows.Scan(&bid) == nil {
				bids = append(bids, bid)
			}
		}
		brows.Close()
		for i, bid := range bids {
			db.Exec(`UPDATE servers SET sort_order = ? WHERE id = ?`, i+1, bid)
		}
	}
	// Load deleted set and override names from SQLite (survives restarts)
	deleted := make(map[string]struct{})
	overrides := make(map[string]string)
	drows, _ := db.Query(`SELECT id FROM deleted_servers`)
	for drows.Next() {
		var did string
		if drows.Scan(&did) == nil {
			deleted[did] = struct{}{}
		}
	}
	drows.Close()
	orows, _ := db.Query(`SELECT id, override_name FROM servers WHERE override_name IS NOT NULL`)
	for orows.Next() {
		var oid, oname string
		if orows.Scan(&oid, &oname) == nil {
			overrides[oid] = oname
		}
	}
	orows.Close()
	return &Store{
		latest:    make(map[string]*proto.State),
		known:     make(map[string]string),
		deleted:   deleted,
		overrides: make(map[string]string),
		db:        db,
	}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS servers (
	id    TEXT PRIMARY KEY,
	name  TEXT NOT NULL,
	first_seen INTEGER NOT NULL,
	override_name TEXT
);
CREATE TABLE IF NOT EXISTS metrics (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	agent_id  TEXT NOT NULL,
	ts        INTEGER NOT NULL,
	cpu       REAL,
	mem_used  INTEGER,
	mem_total INTEGER,
	swap_used INTEGER,
	swap_total INTEGER,
	disk_used INTEGER,
	disk_total INTEGER,
	net_in    INTEGER,
	net_out   INTEGER,
	load1     REAL
);
CREATE INDEX IF NOT EXISTS idx_metrics_agent_ts ON metrics(agent_id, ts);
CREATE TABLE IF NOT EXISTS deleted_servers (
	id TEXT PRIMARY KEY
);
`

// RememberAgent records a known agent id/name in the servers table. If the
// agent reconnects with the same ID, the name is refreshed. Reconnection
// under a different ID (e.g. lost agent.id file) creates a separate record;
// history is NOT migrated — the old entry remains until admin-deleted.
func (s *Store) RememberAgent(id, name string) {
	// Ignore if this agent was admin-deleted
	s.mu.RLock()
	if _, gone := s.deleted[id]; gone {
		s.mu.RUnlock()
		return
	}
	s.mu.RUnlock()

	s.mu.Lock()
	s.known[id] = name
	s.mu.Unlock()
	_, _ = s.db.Exec(
		`INSERT INTO servers(id,name,first_seen,sort_order)
		 VALUES(?,?,?,COALESCE((SELECT MAX(sort_order) FROM servers),0)+1)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name`,
		id, name, time.Now().Unix(),
	)
}

// UpdateState stores a fresh snapshot: updates the hot cache and inserts a row.
func (s *Store) UpdateState(st *proto.State) {
	// Ignore agents that were admin-deleted (prevents re-registration).
	s.mu.RLock()
	if _, gone := s.deleted[st.AgentID]; gone {
		s.mu.RUnlock()
		return
	}
	s.mu.RUnlock()
	// An admin-set name takes precedence over what the agent reports.
	if ov := s.overrideName(st.AgentID); ov != "" {
		st.Name = ov
	}
	s.mu.Lock()
	s.latest[st.AgentID] = st
	s.known[st.AgentID] = st.Name
	s.mu.Unlock()

	_, _ = s.db.Exec(
		`INSERT INTO metrics(agent_id,ts,cpu,mem_used,mem_total,swap_used,swap_total,disk_used,disk_total,net_in,net_out,load1)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		st.AgentID, st.Timestamp,
		st.CPUUsage, st.MemoryUsed, st.MemoryTotal, st.SwapUsed, st.SwapTotal,
		st.DiskUsed, st.DiskTotal, st.NetIn, st.NetOut, st.Load1,
	)
}

// Latest returns all agents' most recent cached states, keyed by id.
func (s *Store) Latest() map[string]*proto.State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]*proto.State, len(s.latest))
	for k, v := range s.latest {
		out[k] = v
	}
	return out
}

// One returns the latest cached state for a single agent.
func (s *Store) One(id string) *proto.State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.latest[id]
}

// MetricRow is one historical sample for charting.
type MetricRow struct {
	TS       int64   `json:"ts"`
	CPU      float64 `json:"cpu"`
	MemUsed  uint64  `json:"mem_used"`
	MemTotal uint64  `json:"mem_total"`
	SwapUsed uint64  `json:"swap_used"`
	DiskUsed uint64  `json:"disk_used"`
	NetIn    uint64  `json:"net_in"`
	NetOut   uint64  `json:"net_out"`
	Load1    float64 `json:"load1"`
}

// History returns the metric samples for an agent since `since` (unix seconds).
func (s *Store) History(id string, since int64) []MetricRow {
	rows, err := s.db.Query(
		`SELECT ts,cpu,mem_used,mem_total,swap_used,disk_used,net_in,net_out,load1
		 FROM metrics WHERE agent_id=? AND ts>=? ORDER BY ts ASC`,
		id, since,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []MetricRow
	for rows.Next() {
		var r MetricRow
		if err := rows.Scan(&r.TS, &r.CPU, &r.MemUsed, &r.MemTotal, &r.SwapUsed,
			&r.DiskUsed, &r.NetIn, &r.NetOut, &r.Load1); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// Servers returns metadata for all known agents.
type ServerMeta struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	FirstSeen int64  `json:"first_seen"`
	SortOrder int    `json:"sort_order"`
}

func (s *Store) Servers() []ServerMeta {
	rows, err := s.db.Query(`SELECT id,COALESCE(override_name,name),first_seen,sort_order FROM servers ORDER BY sort_order, COALESCE(override_name,name)`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []ServerMeta
	for rows.Next() {
		var m ServerMeta
		if err := rows.Scan(&m.ID, &m.Name, &m.FirstSeen, &m.SortOrder); err == nil {
			out = append(out, m)
		}
	}
	return out
}

// Reorder sets a sequential manual sort order for the given agent IDs.
// IDs are persisted in the order provided; any agent not in the list
// keeps its existing (higher) position. This is the backing store for
// drag-to-reorder in the UI.
func (s *Store) Reorder(ids []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE servers SET sort_order = ? WHERE id = ?`, i+1, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Rename sets a display-name override for an agent. This takes precedence over
// the name the agent reports on connect, so renaming survives reconnection.
func (s *Store) Rename(id, name string) error {
	if _, err := s.db.Exec(`UPDATE servers SET override_name=? WHERE id=?`, name, id); err != nil {
		return err
	}
	s.mu.Lock()
	s.overrides[id] = name
	s.mu.Unlock()
	return nil
}

// Delete removes an agent and all its metrics from the database and cache.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	delete(s.latest, id)
	delete(s.known, id)
	delete(s.overrides, id)
	s.deleted[id] = struct{}{} // block re-registration
	s.mu.Unlock()
	// Persist to SQLite so it survives restarts
	_, err := s.db.Exec(`INSERT OR REPLACE INTO deleted_servers(id) VALUES(?)`, id)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM metrics WHERE agent_id=?`, id); err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM servers WHERE id=?`, id)
	return err
}

// overrideName returns the admin-set display name for an agent, if any.
// Uses the in-memory cache to avoid a SQLite query on every UpdateState.
func (s *Store) overrideName(id string) string {
	s.mu.RLock()
	n := s.overrides[id]
	s.mu.RUnlock()
	return n
}

// SetLatestName overrides the name field of the cached latest state for an
// agent, so renders reflect a rename immediately. No-op if unknown.
func (s *Store) SetLatestName(id, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.latest[id]; st != nil {
		st.Name = name
	}
}

// SetLatestNameCopy sets the name atomically and returns a shallow copy
// of the resulting state. The caller can safely JSON-marshal the copy
// without racing a concurrent UpdateState replacing the map entry.
func (s *Store) SetLatestNameCopy(id, name string) *proto.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.latest[id]
	if st == nil {
		return nil
	}
	st.Name = name
	cp := *st
	return &cp
}

// LatestByID returns the cached state for one agent (or nil).
func (s *Store) LatestByID(id string) *proto.State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.latest[id]
}

// Prune deletes metrics older than the history window.
func (s *Store) Prune() {
	cutoff := time.Now().Add(-historyWindow).Unix()
	_, _ = s.db.Exec(`DELETE FROM metrics WHERE ts < ?`, cutoff)
}


// ReapStale removes in-memory latest states whose timestamp is older
// than staleAge (24h). We intentionally keep offline agents' last-known
// state so they remain visible in the list with an "offline" badge
// instead of vanishing. 24h is a safety cap: a genuinely decommissioned
// agent that never reconnects is eventually forgotten from the hot
// cache (its history in SQLite is pruned separately by Prune).
const staleAge = 24 * time.Hour

// ReapStale removes in-memory latest states whose timestamp is older
// than staleAge. Called alongside Prune() on the periodic ticker.
func (s *Store) ReapStale() {
	cutoff := time.Now().Add(-staleAge).Unix()
	s.mu.Lock()
	for id, st := range s.latest {
		if st.Timestamp < cutoff {
			delete(s.latest, id)
		}
	}
	s.mu.Unlock()
}
