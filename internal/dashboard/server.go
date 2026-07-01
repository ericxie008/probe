package dashboard

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// upgrader checks the Origin header to prevent Cross-Site WebSocket Hijacking.
// In production the dashboard should sit behind a reverse proxy that sets
// the correct Host header; we accept same-origin and explicit proxy origins.
var upgrader = websocket.Upgrader{
	CheckOrigin: checkOrigin,
}

func checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // non-browser clients (curl, agent) have no Origin
	}
	host := r.Host
	// allow same-origin: Origin's host must match the request Host
	if strings.Contains(origin, "://"+host) || strings.HasSuffix(origin, host) {
		return true
	}
	// fall back to allowing localhost dev origins
	for _, h := range []string{"localhost", "127.0.0.1", "0.0.0.0"} {
		if strings.Contains(origin, "://"+h) {
			return true
		}
	}
	return false
}

// Server wires the hub, store, and HTTP routes together.
type Server struct {
	hub      *Hub
	store    *Store
	mux      *http.ServeMux
	webToken string
	secret     string
	listenAddr string

	sessMu   sync.Mutex
	sessions map[string]time.Time // token -> expiry

	loginMu    sync.Mutex
	loginFails map[string]*loginBucket // ip -> rate-limit state
}

type loginBucket struct {
	fails    int
	lastFail time.Time
}

// NewServer builds a Server backed by the given store/hub.
func NewServer(store *Store, hub *Hub, webToken, secret, listenAddr string) *Server {
	s := &Server{
		hub: hub, store: store, mux: http.NewServeMux(),
		webToken: webToken, secret: secret, listenAddr: listenAddr,
		sessions: make(map[string]time.Time),
		loginFails: make(map[string]*loginBucket),
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/agent", s.handleAgentWS)
	s.mux.HandleFunc("/ws", s.gateWeb(s.handleViewerWS))
	s.mux.HandleFunc("/api/servers", s.gateWeb(s.handleServers))
	s.mux.HandleFunc("/api/servers/", s.gateWeb(s.handleServerDetail))
	s.mux.HandleFunc("/api/deploy", s.gateWeb(s.handleDeploy))
}

func (s *Server) LoginPageHandler() http.HandlerFunc { return s.handleLoginPage }
func (s *Server) LoginHandler() http.HandlerFunc     { return s.handleAPILogin }
func (s *Server) LogoutHandler() http.HandlerFunc    { return s.handleAPILogout }

// GateStatic wraps the static file server with browser auth.
func (s *Server) GateStatic(h http.Handler) http.Handler {
	if s.webToken == "" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authorized(r, []byte(s.webToken)) {
			if c, err := r.Cookie("probe_session"); err != nil || !s.validSession(c.Value) {
				if t := r.URL.Query().Get("token"); t != "" && subtle.ConstantTimeCompare([]byte(t), []byte(s.webToken)) == 1 {
					http.SetCookie(w, &http.Cookie{
						Name: "probe_session", Value: s.newSession(), Path: "/",
						HttpOnly: true, SameSite: http.SameSiteStrictMode,
						MaxAge: 7 * 24 * 3600, Secure: r.TLS != nil,
					})
				}
			}
			h.ServeHTTP(w, r)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

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
		if r.Header.Get("Authorization") == "" && r.URL.Query().Get("token") == "" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

func (s *Server) authorized(r *http.Request, want []byte) bool {
	if c, err := r.Cookie("probe_session"); err == nil && s.validSession(c.Value) {
		return true
	}
	cand := r.Header.Get("Authorization")
	const pfx = "Bearer "
	if len(cand) >= len(pfx) && cand[:len(pfx)] == pfx {
		return subtle.ConstantTimeCompare([]byte(cand[len(pfx):]), want) == 1
	}
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
	// GC expired sessions opportunistically
	now := time.Now()
	for k, exp := range s.sessions {
		if now.After(exp) {
			delete(s.sessions, k)
		}
	}
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

// loginRateLimit returns false if the IP has exceeded the fail threshold.
const (
	maxLoginFails = 10
	loginBanTime  = 5 * time.Minute
)

func (s *Server) loginRateLimit(ip string) bool {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	b, ok := s.loginFails[ip]
	if !ok {
		b = &loginBucket{}
		s.loginFails[ip] = b
	}
	// reset counter if the ban window expired
	if b.fails >= maxLoginFails && time.Since(b.lastFail) > loginBanTime {
		b.fails = 0
	}
	return b.fails < maxLoginFails
}

func (s *Server) recordLoginFail(ip string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	b, ok := s.loginFails[ip]
	if !ok {
		b = &loginBucket{}
		s.loginFails[ip] = b
	}
	b.fails++
	b.lastFail = time.Now()
}

func (s *Server) recordLoginOK(ip string) {
	s.loginMu.Lock()
	delete(s.loginFails, ip)
	s.loginMu.Unlock()
}

func clientIP(r *http.Request) string {
	// respect X-Forwarded-For from a trusted reverse proxy
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("probe_session"); err == nil && s.validSession(c.Value) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(loginHTML))
}

func (s *Server) handleAPILogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.loginRateLimit(ip) {
		http.Error(w, "尝试过于频繁,请稍后再试", http.StatusTooManyRequests)
		return
	}
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
		s.recordLoginFail(ip)
		w.Header().Set("Retry-After", "300")
		http.Error(w, "密码错误", http.StatusUnauthorized)
		return
	}
	s.recordLoginOK(ip)
	sid := s.newSession()
	http.SetCookie(w, &http.Cookie{
		Name: "probe_session", Value: sid, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		MaxAge: 7 * 24 * 3600, Secure: r.TLS != nil,
	})
	writeJSON(w, map[string]any{"ok": true})
}

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

// GET /api/deploy -> returns connection info for generating agent install commands.
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"secret": s.secret,
	})
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
// GET /api/servers/{id}/history?minutes=60
// POST /api/servers/{id}/rename
func (s *Server) handleServerDetail(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/api/servers/"):]
	detailID, rest := splitFirst(id, "/")
	if rest == "rename" && r.Method == http.MethodPost {
	if rest == "delete" && r.Method == http.MethodPost {
		if err := s.store.Delete(detailID); err != nil {
			http.Error(w, "delete failed", http.StatusInternalServerError)
			return
		}
		s.hub.removeAgent(detailID)
		writeJSON(w, map[string]any{"ok": true})
		return
	}
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
		s.hub.applyRename(detailID, name)
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
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):]
	}
	return s, ""
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) AgentHandler() http.HandlerFunc  { return s.handleAgentWS }
func (s *Server) ViewerHandler() http.HandlerFunc { return s.gateWeb(s.handleViewerWS) }
func (s *Server) APIHandler() http.HandlerFunc {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/deploy" {
			s.handleDeploy(w, r)
			return
		}
		if r.URL.Path == "/api/servers" {
			s.handleServers(w, r)
			return
		}
		if len(r.URL.Path) > len("/api/servers/") && r.URL.Path[:len("/api/servers/")] == "/api/servers/" {
			s.handleServerDetail(w, r)
			return
		}
		http.NotFound(w, r)
	})
	return s.gateWeb(inner)
}
