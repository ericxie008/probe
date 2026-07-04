package dashboard

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"net/http"
	"net/http/httptest"
	"strings"
)

// ---- Store.Reorder / sort_order ----

func serverIDs(metas []ServerMeta) []string {
	out := make([]string, len(metas))
	for i, m := range metas {
		out[i] = m.ID
	}
	return out
}

func TestReorder(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.RememberAgent("a", "Alpha")
	s.RememberAgent("b", "Bravo")
	s.RememberAgent("c", "Charlie")

	// New agents get sort_order MAX+1, so they appear in insertion order.
	got := serverIDs(s.Servers())
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("initial order = %v, want %v", got, want)
	}

	// Reorder to c, a, b and verify persistence.
	if err := s.Reorder([]string{"c", "a", "b"}); err != nil {
		t.Fatalf("Reorder: %v", err)
	}
	got = serverIDs(s.Servers())
	if want := []string{"c", "a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after reorder = %v, want %v", got, want)
	}

	// A reconnecting agent keeps its position (name refresh, no reorder).
	s.RememberAgent("a", "Alpha-Renamed")
	got = serverIDs(s.Servers())
	if want := []string{"c", "a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after reconnect = %v, want %v", got, want)
	}

	// A brand-new agent lands at the end.
	s.RememberAgent("d", "Delta")
	got = serverIDs(s.Servers())
	if want := []string{"c", "a", "b", "d"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after new agent = %v, want %v", got, want)
	}
}

// ---- checkOrigin ----

func TestCheckOrigin(t *testing.T) {
	cases := []struct {
		name   string
		origin string
		host   string
		want   bool
	}{
		{"no origin (non-browser client)", "", "example.com:8553", true},
		{"same origin", "https://example.com:8553", "example.com:8553", true},
		{"same host different port", "https://example.com", "example.com:8553", true},
		{"cross origin", "https://evil.com", "example.com:8553", false},
		{"localhost dev", "http://localhost:3000", "example.com:8553", true},
		{"127.0.0.1 dev", "http://127.0.0.1:3000", "example.com:8553", true},
		{"malformed origin", "://bad", "example.com:8553", false},
		{"empty host spoof", "https://", "example.com:8553", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.Host = c.host
			if c.origin != "" {
				req.Header.Set("Origin", c.origin)
			}
			if got := checkOrigin(req); got != c.want {
				t.Errorf("checkOrigin() = %v, want %v", got, c.want)
			}
		})
	}
}

// ---- checkCSRF ----

func TestCheckCSRF(t *testing.T) {
	cases := []struct {
		name   string
		method string
		origin string
		host   string
		want   bool
	}{
		{"GET always passes", "GET", "", "example.com", true},
		{"POST same origin", "POST", "https://example.com", "example.com:8553", true},
		{"POST cross origin", "POST", "https://evil.com", "example.com:8553", false},
		{"POST no origin blocked", "POST", "", "example.com:8553", false},
		{"POST malformed origin", "POST", "://bad", "example.com:8553", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, "/api/servers/x/rename", nil)
			req.Host = c.host
			if c.origin != "" {
				req.Header.Set("Origin", c.origin)
			}
			if got := checkCSRF(req); got != c.want {
				t.Errorf("checkCSRF() = %v, want %v", got, c.want)
			}
		})
	}
}

// ---- clientIP ----

func TestClientIP(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		xff    string
		want   string
	}{
		{"direct connection", "203.0.113.5:54321", "", "203.0.113.5"},
		{"localhost trusts xff", "127.0.0.1:12345", "198.51.100.7", "198.51.100.7"},
		{"non-localhost ignores xff", "203.0.113.5:1234", "10.0.0.1", "203.0.113.5"},
		{"localhost no xff", "127.0.0.1:12345", "", "127.0.0.1"},
		{"localhost multi-xff uses first", "127.0.0.1:12345", "198.51.100.7, 10.0.0.1", "198.51.100.7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = c.remote
			if c.xff != "" {
				req.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := clientIP(req); got != c.want {
				t.Errorf("clientIP() = %q, want %q", got, c.want)
			}
		})
	}
}

// ---- splitFirst ----

func TestSplitFirst(t *testing.T) {
	cases := []struct {
		in    string
		sep   string
		wantA string
		wantB string
	}{
		{"abc/def", "/", "abc", "def"},
		{"abc", "/", "abc", ""},
		{"abc/def/ghi", "/", "abc", "def/ghi"},
		{"abc--def--ghi", "--", "abc", "def--ghi"},
		{"", "/", "", ""},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			a, b := splitFirst(c.in, c.sep)
			if a != c.wantA || b != c.wantB {
				t.Errorf("splitFirst(%q, %q) = (%q, %q), want (%q, %q)",
					c.in, c.sep, a, b, c.wantA, c.wantB)
			}
		})
	}
}

// ---- login rate limiting ----

func TestLoginRateLimit(t *testing.T) {
	s := &Server{loginFails: make(map[string]*loginBucket)}

	ip := "198.51.100.42"
	for i := 0; i < maxLoginFails; i++ {
		if !s.loginRateLimit(ip) {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
		s.recordLoginFail(ip)
	}
	if s.loginRateLimit(ip) {
		t.Fatal("should be rate-limited after exceeding fail threshold")
	}
	s.recordLoginOK(ip)
	if !s.loginRateLimit(ip) {
		t.Fatal("should be allowed after successful login resets counter")
	}
}

// ---- session lifecycle ----

func TestSessionLifecycle(t *testing.T) {
	s := &Server{sessions: make(map[string]time.Time)}
	sid := s.newSession()
	if sid == "" {
		t.Fatal("newSession returned empty id")
	}
	if !s.validSession(sid) {
		t.Fatal("valid session should be valid")
	}
	s.dropSession(sid)
	if s.validSession(sid) {
		t.Fatal("dropped session should be invalid")
	}
}

// ---- authorized: cookie / bearer / query token paths ----

func newTestServer(t *testing.T) *Server {
	t.Helper()
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	hub := NewHub(store, "testsecret")
	return NewServer(store, hub, "webtok123", "testsecret", "127.0.0.1:0")
}

func TestAuthorizedPaths(t *testing.T) {
	s := newTestServer(t)
	want := []byte(s.webToken)

	req := httptest.NewRequest("GET", "/api/servers", nil)
	if s.authorized(req, want) {
		t.Fatal("should reject request with no credentials")
	}

	sid := s.newSession()
	req2 := httptest.NewRequest("GET", "/api/servers", nil)
	req2.AddCookie(&http.Cookie{Name: "probe_session", Value: sid})
	if !s.authorized(req2, want) {
		t.Fatal("should accept valid session cookie")
	}

	req3 := httptest.NewRequest("GET", "/api/servers", nil)
	req3.AddCookie(&http.Cookie{Name: "probe_session", Value: "deadbeef"})
	if s.authorized(req3, want) {
		t.Fatal("should reject bogus session cookie")
	}

	req4 := httptest.NewRequest("GET", "/api/servers", nil)
	req4.Header.Set("Authorization", "Bearer "+s.webToken)
	if !s.authorized(req4, want) {
		t.Fatal("should accept valid bearer token")
	}

	req5 := httptest.NewRequest("GET", "/api/servers", nil)
	req5.Header.Set("Authorization", "Bearer wrongtoken")
	if s.authorized(req5, want) {
		t.Fatal("should reject wrong bearer token")
	}

	req6 := httptest.NewRequest("GET", "/api/servers?token="+s.webToken, nil)
	if !s.authorized(req6, want) {
		t.Fatal("should accept valid query token")
	}

	req7 := httptest.NewRequest("GET", "/api/servers?token=wrongtoken", nil)
	if s.authorized(req7, want) {
		t.Fatal("should reject wrong query token")
	}
}

// ---- GateStatic: token-in-URL is exchanged for a cookie and stripped ----

func TestGateStaticStripsTokenFromURL(t *testing.T) {
	s := newTestServer(t)
	h := s.GateStatic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "" {
			t.Error("inner handler saw token in URL")
		}
	}))

	req := httptest.NewRequest("GET", "/?token="+s.webToken, nil)
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if strings.Contains(loc, "token=") {
		t.Errorf("redirect Location still contains token: %q", loc)
	}
	if loc != "/" {
		t.Errorf("expected redirect to '/', got %q", loc)
	}

	var hasSession bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == "probe_session" && c.Value != "" {
			hasSession = true
		}
	}
	if !hasSession {
		t.Error("no session cookie set after token exchange")
	}
}

func TestGateStaticUnauthenticatedRedirectsToLogin(t *testing.T) {
	s := newTestServer(t)
	h := s.GateStatic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("inner handler should not be called when unauthenticated")
	}))
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect to login, got %d", rec.Code)
	}
	if rec.Header().Get("Location") != "/login" {
		t.Errorf("expected redirect to /login, got %q", rec.Header().Get("Location"))
	}
}

// ---- handleServerDetail: malformed ID rejection ----

func TestHandleServerDetailRejectsTraversal(t *testing.T) {
	s := newTestServer(t)
	cases := []string{
		"/api/servers/..%2f..%2f/etc/passwd",
		"/api/servers/a/b",
		"/api/servers/a\\b",
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			req := httptest.NewRequest("GET", p, nil)
			req.AddCookie(&http.Cookie{Name: "probe_session", Value: s.newSession()})
			req.Host = "example.com"
			rec := httptest.NewRecorder()
			s.handleServerDetail(rec, req)
			if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
				t.Errorf("for path %q got status %d, want 400 or 404", p, rec.Code)
			}
		})
	}
}
