package agent

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"probe/internal/proto"
)

// Config controls the agent runtime behaviour.
type Config struct {
	Server   string // dashboard host:port, e.g. "127.0.0.1:8000"
	Token    string // secret shared with the dashboard
	Name     string // display name for this host
	AgentID  string // stable id; generated if empty
	Interval time.Duration
	TLS      bool // connect with wss:// (TLS) instead of ws://
	Insecure bool // skip TLS cert verification (self-signed / private net)
}

// errAuthFailed signals that the server rejected the token.
var errAuthFailed = errors.New("authentication failed")

// Client is the agent: it connects to the dashboard, authenticates, and
// periodically streams host metrics. It is read-only on the server side
// (no remote command execution), which keeps the attack surface small.
type Client struct {
	cfg       Config
	collector *Collector
	conn      *websocket.Conn
}

// NewClient builds an agent runtime (does not connect yet).
func NewClient(cfg Config) *Client {
	if cfg.AgentID == "" {
		cfg.AgentID = loadOrCreateID()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 3 * time.Second
	}
	return &Client{
		cfg:       cfg,
		collector: NewCollector(),
	}
}

// Run connects and streams metrics forever, reconnecting with backoff.
func (c *Client) Run() {
	backoff := time.Second
	authFails := 0
	const maxAuthRetries = 5
	for {
		err := c.runOnce()
		if err != nil {
			if errors.Is(err, errAuthFailed) {
				authFails++
				if authFails >= maxAuthRetries {
					log.Fatalf("authentication failed %d times, check token. exiting.", authFails)
				}
				log.Printf("authentication rejected (%d/%d), will retry in %v: %v",
					authFails, maxAuthRetries, backoff, err)
			} else {
				authFails = 0 // any non-auth error resets the auth-fail counter
				log.Printf("agent disconnected: %v", err)
			}
		}
		time.Sleep(backoff)
		if backoff < 60*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) runOnce() error {
	scheme := "ws"
	if c.cfg.TLS {
		scheme = "wss"
	}
	u := url.URL{Scheme: scheme, Host: c.cfg.Server, Path: "/agent"}
	header := http.Header{}
	header.Set("User-Agent", "probe-agent/1.0 ("+runtime.GOOS+"/"+runtime.GOARCH+")")

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	if c.cfg.TLS {
		dialer.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		if c.cfg.Insecure {
			dialer.TLSClientConfig.InsecureSkipVerify = true
		}
	}
	conn, _, err := dialer.Dial(u.String(), header)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	c.conn = conn
	defer conn.Close()
	log.Printf("connected to %s", u.String())

	// Authenticate first.
	auth := proto.Message{
		Type: proto.MsgAuth,
		Auth: &proto.AuthPayload{
			Token:   c.cfg.Token,
			Name:    c.cfg.Name,
			AgentID: c.cfg.AgentID,
		},
	}
	if err := writeJSON(conn, auth); err != nil {
		return fmt.Errorf("auth send: %w", err)
	}

	// Read the auth result before streaming.
	var ar proto.Message
	if err := readJSON(conn, &ar); err != nil {
		return fmt.Errorf("auth read: %w", err)
	}
	if ar.AuthResult == nil || !ar.AuthResult.OK {
		msg := "unknown"
		if ar.AuthResult != nil {
			msg = ar.AuthResult.Message
		}
		log.Printf("authentication rejected: %s", msg)
		return errAuthFailed
	}
	log.Printf("authenticated as %s (id=%s)", c.cfg.Name, c.cfg.AgentID)

	// drainLoop keeps reading so we notice when the connection drops; the
	// server only sends the initial auth result and otherwise pushes nothing.
	// We set a Pong handler + read deadline so a silently-dropped connection
	// (NAT timeout, suspended host) is detected within the deadline window
	// instead of blocking indefinitely until the next TCP RST.
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})
	_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	done := make(chan struct{})
	go func() {
		defer close(done)
		var msg proto.Message
		for {
			if err := readJSON(conn, &msg); err != nil {
				log.Printf("read: %v", err)
				return
			}
			// No inbound message types are handled: metrics flow one way.
		}
	}()

	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()
	// Send a WebSocket Ping every 30s to keep NAT entries warm and to
	// refresh the server's read deadline via the pong round-trip.
	pingTicker := time.NewTicker(30 * time.Second)
	defer pingTicker.Stop()
	for {
		select {
		case <-ticker.C:
			state := c.collector.Collect(c.cfg.AgentID, c.cfg.Name)
			if err := writeJSON(conn, proto.Message{Type: proto.MsgState, State: state}); err != nil {
				return fmt.Errorf("state send: %w", err)
			}
		case <-done:
			return nil
		case <-pingTicker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return fmt.Errorf("ping send: %w", err)
			}
		}
	}
}

func writeJSON(c *websocket.Conn, m proto.Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	// Guard against a stalled network: if the write blocks longer than
	// 5s the connection is effectively dead and should be recycled.
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.WriteMessage(websocket.TextMessage, b)
}

func readJSON(c *websocket.Conn, m *proto.Message) error {
	_, b, err := c.ReadMessage()
	if err != nil {
		return err
	}
	return json.Unmarshal(b, m)
}

// loadOrCreateID persists a stable agent ID next to the binary so it survives
// restarts. Without this every reconnect creates a ghost entry on the
// dashboard and breaks the rename / history linkage.
func loadOrCreateID() string {
	for _, dir := range []string{
		os.Getenv("PROBE_DATA_DIR"),
		filepath.Dir(os.Args[0]),
		".",
		"/var/lib/probe-agent",
		"/opt/probe-agent",
	} {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, "agent.id")
		if b, err := os.ReadFile(p); err == nil {
			id := strings.TrimSpace(string(b))
			if len(id) >= 8 {
				return id
			}
		}
	}
	// No existing ID found in any candidate dir: create one in the first
	// writable location. Scanning all dirs first avoids creating a
	// duplicate ID in an early dir when a valid one exists in a later dir.
	for _, dir := range []string{
		os.Getenv("PROBE_DATA_DIR"),
		filepath.Dir(os.Args[0]),
		".",
		"/var/lib/probe-agent",
		"/opt/probe-agent",
	} {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, "agent.id")
		if id := uuid.NewString(); os.WriteFile(p, []byte(id), 0600) == nil {
			return id
		}
	}
	// Last resort: nothing writable, return an ephemeral ID.
	return uuid.NewString()
}
