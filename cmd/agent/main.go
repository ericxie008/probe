package main

import (
	"flag"
	"log"
	"os"
	"time"

	"probe/internal/agent"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

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
