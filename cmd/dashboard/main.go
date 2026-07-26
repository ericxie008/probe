package main

import (
	"context"
	"flag"
	"log"
	"net"
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
	// 优先显示非环回 IP,部署在远程服务器时日志里的地址直接可用。
	// 找不到非环回地址时回退到 host:port 原样显示。
	host := displayHost(*addr)
	if pub := primaryIP(); pub != "" {
		host = pub + portSuffix(*addr)
	}
	log.Printf("dashboard listening on %s (web ui at %s://%s/)", *addr, scheme, host)

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

// portSuffix extracts the ":port" portion of a "host:port" listen address.
func portSuffix(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i:]
	}
	return ""
}

// primaryIP returns the first non-loopback IPv4 of this machine, or "".
// Used only for the startup log so the operator sees a usable URL when
// deploying on a remote box where "localhost" in the log is useless.
func primaryIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		if v4 := ipNet.IP.To4(); v4 != nil {
			return v4.String()
		}
	}
	return ""
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
			"script-src 'self'; "+
			"style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data:; "+
			"connect-src 'self'; "+
			"font-src 'self'; "+
			"object-src 'none'; base-uri 'self'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// 静态资源(html/css/js)用 no-cache:浏览器可以缓存,但每次使用前必须回源校验。
	// FileServer 会自动带 Last-Modified/ETag,未改动时回 304 几乎零开销;改动后返回新内容。
	// 这解决了 iOS Safari 对无显式缓存头资源做激进启发式缓存、导致前端更新后普通刷新
	// 仍读旧文件的问题(此前 app.js 的在线状态修复在普通窗口里不生效)。
	w.Header().Set("Cache-Control", "no-cache")
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
