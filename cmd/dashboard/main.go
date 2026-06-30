package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"probe/internal/dashboard"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// Default to localhost so the dashboard is not exposed to the network by
	// accident; operators who want remote access should set -addr ":8000" and
	// enable TLS below (or put it behind a TLS reverse proxy).
	addr := flag.String("addr", env("PROBE_ADDR", "127.0.0.1:8000"), "listen address")
	secret := flag.String("secret", env("PROBE_SECRET", ""), "shared agent auth token")
	webToken := flag.String("web-token", env("PROBE_WEB_TOKEN", ""), "access token for the web UI / API (empty = open, rely on reverse-proxy auth)")
	dbPath := flag.String("db", env("PROBE_DB", "data/probe.db"), "sqlite database path")
	cert := flag.String("tls-cert", env("PROBE_TLS_CERT", ""), "path to TLS certificate (enables wss/https)")
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
	defer store.Close()

	// Periodically prune old metric rows so the DB stays bounded.
	go func() {
		t := time.NewTicker(10 * time.Minute)
		for range t.C {
			store.Prune()
		}
	}()

	hub := dashboard.NewHub(store, *secret)
	api := dashboard.NewServer(store, hub, *webToken)

	mux := http.NewServeMux()
	mux.HandleFunc("/agent", api.AgentHandler())
	mux.HandleFunc("/login", api.LoginPageHandler())
	mux.HandleFunc("/api/login", api.LoginHandler())
	mux.HandleFunc("/api/logout", api.LogoutHandler())
	mux.HandleFunc("/ws", api.ViewerHandler())
	mux.HandleFunc("/api/", api.APIHandler())
	mux.Handle("/", api.GateStatic(safeStatic(http.FileServer(http.Dir("web")))))

	scheme := "http"
	if *cert != "" {
		scheme = "https"
	}
	log.Printf("dashboard listening on %s (web ui at %s://%s/)", *addr, scheme, displayHost(*addr))
	if *cert != "" {
		if err := http.ListenAndServeTLS(*addr, *cert, *key, mux); err != nil {
			log.Fatal(err)
		}
	} else {
		if err := http.ListenAndServe(*addr, mux); err != nil {
			log.Fatal(err)
		}
	}
}

// displayHost keeps ":port" -> "localhost" so the logged URL is clickable.
func displayHost(addr string) string {
	for i := 0; i < len(addr); i++ {
		if addr[i] == ':' {
			return "localhost" + addr[i:]
		}
	}
	return addr
}

// safeStatic wraps a file server with defensive response headers for the UI.
func safeStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		h.ServeHTTP(w, r)
	})
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
