package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"probe/internal/dashboard"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	addr := flag.String("addr", env("PROBE_ADDR", "127.0.0.1:8000"), "listen address")
	secret := flag.String("secret", env("PROBE_SECRET", ""), "shared agent auth token")
	webToken := flag.String("web-token", env("PROBE_WEB_TOKEN", ""), "access token for the web UI / API (empty = open)")
	dbPath := flag.String("db", env("PROBE_DB", "data/probe.db"), "sqlite database path")
	cert := flag.String("tls-cert", env("PROBE_TLS_CERT", ""), "path to TLS certificate")
	key := flag.String("tls-key", env("PROBE_TLS_KEY", ""), "path to TLS private key")
	flag.Parse()

	if *secret == "" {
		log.Fatal("secret is required (use -secret or PROBE_SECRET)")
	}
	if (*cert == "") != (*key == "") {
		log.Fatal("-tls-cert and -tls-key must be set together")
	}

	store, err := dashboard.NewStore(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}

	go func() {
		t := time.NewTicker(10 * time.Minute)
		for range t.C {
			store.Prune()
			store.ReapStale()
		}
	}()

	hub := dashboard.NewHub(store, *secret)
	api := dashboard.NewServer(store, hub, *webToken, *secret, *addr)

	mux := http.NewServeMux()
	mux.HandleFunc("/agent", api.AgentHandler())
	mux.HandleFunc("/login", api.LoginPageHandler())
	mux.HandleFunc("/api/login", api.LoginHandler())
	mux.HandleFunc("/api/logout", api.LogoutHandler())
	mux.HandleFunc("/ws", api.ViewerHandler())
	mux.HandleFunc("/api/", api.APIHandler())
	mux.Handle("/", api.GateStatic(safeStatic(http.FileServer(http.Dir("web")))))

	srv := &http.Server{Addr: *addr, Handler: mux}
	// ReadTimeout is enforced per-read via SetReadDeadline inside the WS
	// handlers, but a server-level timeout here would also kill idle
	// WebSocket upgrades after 30s. WebSocket connections are long-lived,
	// so we do NOT set a server-level WriteTimeout — gorilla/websocket
	// manages its own per-write deadlines.
	srv.ReadTimeout = 30 * time.Second
	srv.IdleTimeout = 120 * time.Second

	// Graceful shutdown on SIGTERM / SIGINT (systemd stop, Ctrl-C).
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		log.Printf("shutting down gracefully...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		store.Close()
	}()

	scheme := "http"
	if *cert != "" {
		scheme = "https"
	}
	log.Printf("dashboard listening on %s (web ui at %s://%s/)", *addr, scheme, displayHost(*addr))

	var serveErr error
	if *cert != "" {
		serveErr = srv.ListenAndServeTLS(*cert, *key)
	} else {
		serveErr = srv.ListenAndServe()
	}
	if serveErr != nil && serveErr != http.ErrServerClosed {
		log.Fatal(serveErr)
	}
	log.Printf("server stopped")
}

func displayHost(addr string) string {
	for i := 0; i < len(addr); i++ {
		if addr[i] == ':' {
			return "localhost" + addr[i:]
		}
	}
	return addr
}

func safeStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w, r)
		// Reject path traversal attempts explicitly. Go's http.FileServer
		// already cleans paths internally, but this adds a second layer
		// for defence-in-depth (e.g. behind proxies that decode %2e).
		cleaned := path.Clean(r.URL.Path)
		if cleaned != r.URL.Path && cleaned != r.URL.Path+"/" {
			if strings.Contains(r.URL.Path, "..") {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// setSecurityHeaders applies CSP, HSTS, and other protective headers.
// CSP allows only self-origin scripts/styles plus the Chart.js CDN;
// HSTS is only set when the connection is over TLS.
func setSecurityHeaders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; "+
			"script-src 'self' https://cdn.jsdelivr.net; "+
			"style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data:; "+
			"connect-src 'self' wss: ws:; "+
			"font-src 'self'; "+
			"object-src 'none'; base-uri 'self'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.TLS != nil {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
