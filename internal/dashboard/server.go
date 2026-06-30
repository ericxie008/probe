package dashboard

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
	"strings"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Server wires the hub, store, and HTTP routes together.
type Server struct {
	hub      *Hub
	store    *Store
	mux      *http.ServeMux
	webToken string

	sessMu  sync.Mutex
	sessions map[string]time.Time // token -> expiry
}

// NewServer builds a Server backed by the given store/hub.
// `webToken` gates the browser surface (REST + viewer WS + UI). Empty means
// no gating (e.g. when behind a reverse proxy that handles auth). The
// agent endpoint /agent is always gated by the hub's own secret.
func NewServer(store *Store, hub *Hub, webToken string) *Server {
	s := &Server{hub: hub, store: store, mux: http.NewServeMux(), webToken: webToken, sessions: make(map[string]time.Time)}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/agent", s.handleAgentWS)
	s.mux.HandleFunc("/ws", s.gateWeb(s.handleViewerWS))
	s.mux.HandleFunc("/api/servers", s.gateWeb(s.handleServers))
	s.mux.HandleFunc("/api/servers/", s.gateWeb(s.handleServerDetail))
}
// LoginPageHandler returns the /login HTML page (always open; it checks
// for an existing session and bounces to "/" if already logged in).
func (s *Server) LoginPageHandler() http.HandlerFunc { return s.handleLoginPage }

// LoginHandler validates the password and sets a session cookie.
func (s *Server) LoginHandler() http.HandlerFunc { return s.handleAPILogin }

// LogoutHandler clears the session cookie.
func (s *Server) LogoutHandler() http.HandlerFunc { return s.handleAPILogout }

// GateStatic wraps an arbitrary handler (e.g. the static file server) with
// the same browser auth used for the API: a valid session cookie lets the
// request through, otherwise it redirects to /login. When no webToken is
// configured it is a no-op pass-through.
func (s *Server) GateStatic(h http.Handler) http.Handler {
	if s.webToken == "" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authorized(r, []byte(s.webToken)) {
			h.ServeHTTP(w, r)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

// gateWeb wraps a handler with the browser access token when configured.
// Accepts a valid session cookie, or a ?token=<token> query param (for the
// WS path from a freshly-logged-in page), or an Authorization: Bearer header.
func (s *Server) gateWeb(next http.HandlerFunc) http.HandlerFunc {
	if s.webToken == "" {
		return next
	}
	want := []byte(s.webToken)
	return func(w http.ResponseWriter, r *http.Request) {
		if s.authorized(r, want) {
			next(w, r)
			return
		}
		// Browser navigations: redirect to login instead of returning 401.
		if r.Header.Get("Authorization") == "" && r.URL.Query().Get("token") == "" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

// authorized checks session cookie / bearer / query token.
func (s *Server) authorized(r *http.Request, want []byte) bool {
	// 1. session cookie
	if c, err := r.Cookie("probe_session"); err == nil && s.validSession(c.Value) {
		return true
	}
	// 2. bearer header
	cand := r.Header.Get("Authorization")
	const pfx = "Bearer "
	if len(cand) >= len(pfx) && cand[:len(pfx)] == pfx {
		return subtle.ConstantTimeCompare([]byte(cand[len(pfx):]), want) == 1
	}
	// 3. query token (for WS handshake before cookie is set, or direct API use)
	if t := r.URL.Query().Get("token"); t != "" {
		return subtle.ConstantTimeCompare([]byte(t), want) == 1
	}
	return false
}

func (s *Server) newSession() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	s.sessMu.Lock()
	s.sessions[id] = time.Now().Add(7 * 24 * time.Hour)
	s.sessMu.Unlock()
	return id
}

func (s *Server) validSession(id string) bool {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	exp, ok := s.sessions[id]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.sessions, id)
		return false
	}
	return true
}

func (s *Server) dropSession(id string) {
	s.sessMu.Lock()
	delete(s.sessions, id)
	s.sessMu.Unlock()
}

// handleLoginPage serves the login form (HTML).
func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	// Already logged in -> bounce to app.
	if c, err := r.Cookie("probe_session"); err == nil && s.validSession(c.Value) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(loginHTML))
}

// handleAPILogin validates the password, sets a session cookie.
func (s *Server) handleAPILogin(w http.ResponseWriter, r *http.Request) {
	want := []byte(s.webToken)
	got := ""
	if r.Header.Get("Content-Type") == "application/json" {
		var body struct{ Password string `json:"password"` }
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			got = body.Password
		}
	} else {
		got = r.FormValue("password")
	}
	if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
		http.Error(w, "密码错误", http.StatusUnauthorized)
		return
	}
	sid := s.newSession()
	http.SetCookie(w, &http.Cookie{
		Name:     "probe_session",
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   7 * 24 * 3600,
		Secure:   r.TLS != nil,
	})
	writeJSON(w, map[string]any{"ok": true})
}

// handleAPILogout clears the session cookie.
func (s *Server) handleAPILogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("probe_session"); err == nil {
		s.dropSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "probe_session", Path: "/", MaxAge: -1})
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleAgentWS(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("agent upgrade: %v", err)
		return
	}
	s.hub.HandleAgent(ws)
}

func (s *Server) handleViewerWS(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("viewer upgrade: %v", err)
		return
	}
	s.hub.HandleViewer(ws)
}

// GET /api/servers -> all known servers with latest state.
func (s *Server) handleServers(w http.ResponseWriter, r *http.Request) {
	servers := s.store.Servers()
	latest := s.store.Latest()
	type out struct {
		ID          string  `json:"id"`
		Name        string  `json:"name"`
		Online      bool    `json:"online"`
		CPUUsage    float64 `json:"cpu_usage"`
		MemUsed     uint64  `json:"mem_used"`
		MemTotal    uint64  `json:"mem_total"`
		DiskUsed    uint64  `json:"disk_used"`
		DiskTotal   uint64  `json:"disk_total"`
		NetSpeedIn  uint64  `json:"net_speed_in"`
		NetSpeedOut uint64  `json:"net_speed_out"`
		OS          string  `json:"os"`
		Uptime      uint64  `json:"uptime"`
		Load1       float64 `json:"load1"`
		Updated     int64   `json:"updated"`
	}
	list := make([]out, 0, len(servers))
	for _, sv := range servers {
		row := out{ID: sv.ID, Name: sv.Name}
		if st := latest[sv.ID]; st != nil {
			row.Online = true
			row.CPUUsage = st.CPUUsage
			row.MemUsed = st.MemoryUsed
			row.MemTotal = st.MemoryTotal
			row.DiskUsed = st.DiskUsed
			row.DiskTotal = st.DiskTotal
			row.NetSpeedIn = st.NetSpeedIn
			row.NetSpeedOut = st.NetSpeedOut
			row.OS = st.OS
			row.Uptime = st.Uptime
			row.Load1 = st.Load1
			row.Updated = st.Timestamp
		}
		list = append(list, row)
	}
	writeJSON(w, list)
}

// GET /api/servers/{id} -> latest full state
// GET /api/servers/{id}/history?minutes=60 -> chart samples
func (s *Server) handleServerDetail(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/api/servers/"):]
	detailID, rest := splitFirst(id, "/")
	// POST /api/servers/{id}/rename  {"name":"新名称"}
	if rest == "rename" && r.Method == http.MethodPost {
		var body struct{ Name string `json:"name"` }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		name := strings.TrimSpace(body.Name)
		if len([]rune(name)) > 64 {
			http.Error(w, "name too long", http.StatusBadRequest)
			return
		}
		if err := s.store.Rename(detailID, name); err != nil {
			http.Error(w, "rename failed", http.StatusInternalServerError)
			return
		}
		s.hub.applyRename(detailID, name) // refresh in-memory cache immediately
		writeJSON(w, map[string]any{"ok": true, "name": name})
		return
	}
	if rest == "history" {
		minutes, _ := strconv.Atoi(r.URL.Query().Get("minutes"))
		if minutes <= 0 || minutes > 24*60 {
			minutes = 60
		}
		since := time.Now().Add(-time.Duration(minutes) * time.Minute).Unix()
		writeJSON(w, s.store.History(detailID, since))
		return
	}
	st := s.store.One(detailID)
	if st == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, st)
}

func splitFirst(s, sep string) (a, b string) {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):]
		}
	}
	return s, ""
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(v)
}

// AgentHandler returns the websocket endpoint for agents.
func (s *Server) AgentHandler() http.HandlerFunc { return s.handleAgentWS }

// ViewerHandler returns the websocket endpoint for browsers.
func (s *Server) ViewerHandler() http.HandlerFunc { return s.gateWeb(s.handleViewerWS) }

// APIHandler returns the REST API handler (matches /api/*).
func (s *Server) APIHandler() http.HandlerFunc {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/servers" {
			s.handleServers(w, r)
			return
		}
		if len(r.URL.Path) > len("/api/servers/") && r.URL.Path[:len("/api/servers/")] == "/api/servers/" {
			s.handleServerDetail(w, r)
			return
		}
		// login/logout 不经 gateWeb,已在路由里单独注册
		http.NotFound(w, r)
	})
	return s.gateWeb(inner)
}
