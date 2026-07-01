package dashboard

import (
	"database/sql"
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
	mu     sync.RWMutex
	latest map[string]*proto.State // agentID -> most recent state
	known   map[string]string       // agentID -> name
	deleted map[string]struct{}       // admin-deleted agentIDs
	db     *sql.DB
}

// NewStore opens (or creates) the SQLite database and ensures tables exist.
func NewStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // sqlite serializes writes anyway
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return &Store{
		latest:  make(map[string]*proto.State),
		known:   make(map[string]string),
		deleted: make(map[string]struct{}),
		db:     db,
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
`

// RememberAgent records a known agent id/name.
func (s *Store) RememberAgent(id, name string) {
	s.mu.Lock()
	s.known[id] = name
	s.mu.Unlock()
	_, _ = s.db.Exec(
		`INSERT INTO servers(id,name,first_seen) VALUES(?,?,?)
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
	TS        int64   `json:"ts"`
	CPU       float64 `json:"cpu"`
	MemUsed   uint64  `json:"mem_used"`
	MemTotal  uint64  `json:"mem_total"`
	SwapUsed  uint64  `json:"swap_used"`
	DiskUsed  uint64  `json:"disk_used"`
	NetIn     uint64  `json:"net_in"`
	NetOut    uint64  `json:"net_out"`
	Load1     float64 `json:"load1"`
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
}

func (s *Store) Servers() []ServerMeta {
	rows, err := s.db.Query(`SELECT id,COALESCE(override_name,name),first_seen FROM servers ORDER BY name`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []ServerMeta
	for rows.Next() {
		var m ServerMeta
		if err := rows.Scan(&m.ID, &m.Name, &m.FirstSeen); err == nil {
			out = append(out, m)
		}
	}
	return out
}
// Rename sets a display-name override for an agent. This takes precedence over
// the name the agent reports on connect, so renaming survives reconnection.
func (s *Store) Rename(id, name string) error {
	_, err := s.db.Exec(`UPDATE servers SET override_name=? WHERE id=?`, name, id)
	return err
}

// Delete removes an agent and all its metrics from the database and cache.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	delete(s.latest, id)
	delete(s.known, id)
	s.deleted[id] = struct{}{} // block re-registration
	s.mu.Unlock()
	if _, err := s.db.Exec(`DELETE FROM metrics WHERE agent_id=?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM servers WHERE id=?`, id)
	return err
}
// overrideName returns the admin-set display name for an agent, if any.
func (s *Store) overrideName(id string) string {
	var n string
	if err := s.db.QueryRow(`SELECT override_name FROM servers WHERE id=?`, id).Scan(&n); err != nil {
		return ""
	}
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
