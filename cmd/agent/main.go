package main

import (
	"flag"
	"log"
	"os"
	"runtime"
	"runtime/debug"
	"time"

	"probe/internal/agent"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// On resource-constrained devices (OpenWrt routers with 128-256MB
	// RAM), raise GC aggressiveness to keep the agent's heap small.
	// The default GOGC=100 triggers GC when heap doubles; lowering it
	// to 50 keeps the peak memory footprint lower at the cost of more
	// frequent (but cheaper) collections.
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(50)
	}

	server := flag.String("server", env("PROBE_SERVER", "127.0.0.1:8000"), "dashboard host:port")
	token := flag.String("token", env("PROBE_TOKEN", ""), "authentication token")
	name := flag.String("name", env("PROBE_NAME", hostname()), "display name")
	id := flag.String("id", env("PROBE_ID", ""), "stable agent id")
	interval := flag.Duration("interval", 3*time.Second, "collection interval")
	tls := flag.Bool("tls", os.Getenv("PROBE_TLS") != "", "connect with TLS (wss://)")
	insecure := flag.Bool("insecure", os.Getenv("PROBE_INSECURE") != "", "skip TLS certificate verification (self-signed)")
	flag.Parse()

	if *token == "" {
		log.Fatal("token is required (use -token or PROBE_TOKEN)")
	}

	cfg := agent.Config{
		Server:   *server,
		Token:    *token,
		Name:     *name,
		AgentID:  *id,
		Interval: *interval,
		TLS:      *tls,
		Insecure: *insecure,
	}
	// Cap to 2 goroutine threads max — the agent is mostly I/O bound
	// and doesn't benefit from many parallel threads on weak CPUs.
	if runtime.GOMAXPROCS(0) > 2 {
		runtime.GOMAXPROCS(2)
	}
	scheme := "ws"
	if *tls {
		scheme = "wss"
	}
	log.Printf("starting agent name=%s server=%s scheme=%s", cfg.Name, cfg.Server, scheme)
	agent.NewClient(cfg).Run()
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "agent"
	}
	return h
}
