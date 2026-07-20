package dashboard

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"probe/internal/proto"
)

// stateMsg is a typed JSON wrapper for broadcasting state to browsers.
// Using a struct avoids map[string]any reflection on every serialisation.
type stateMsg struct {
	Type   string       `json:"type"`
	Online bool         `json:"online"`
	Data   *proto.State `json:"data"`
}

// onlineThresholdSec is how recently an agent must have reported to count
// as online. Shared by the HTTP API and the live WS push so the two never
// disagree. The agent reports every ~3s, so 30s is ~10 missed reports -
// comfortably past jitter, tight enough to flag real outages quickly.
const onlineThresholdSec = 30

// marshalState serialises a state for a browser push, attaching a
// server-authoritative online flag. Computing online here (rather than in
// the browser) removes client/server clock skew: the browser used to do
// Date.now() - s.timestamp, which flickered when the device clock drifted
// relative to the server (notably iPhones whose clock differs from the
// server by tens of seconds).
func marshalState(st *proto.State) []byte {
	online := st != nil && time.Now().Unix()-st.Timestamp < onlineThresholdSec
	b, _ := json.Marshal(stateMsg{Type: "state", Online: online, Data: st})
	return b
}

// Hub owns all agent connections and browser connections. Agents are
// one-way (they only stream state upstream); browsers receive live
// state pushes. There is no command/exec channel, by design.
type Hub struct {
	mu sync.RWMutex

	// agent conns keyed by agent id
	agents map[string]*agentConn
	// browser viewers
	viewers map[*viewerConn]struct{}

	store  *Store
	secret []byte // shared token for agent auth (compared in constant time)
}

type agentConn struct {
	id   string
	name string
	ws   *websocket.Conn
}

type viewerConn struct {
	ws *websocket.Conn
	// mu 串行化对 ws 的所有写。gorilla/websocket 要求同一连接同时只能有一个
	// writer,TextMessage 写与 PingMessage(WriteControl)走不同内部路径,
	// 二者并发会触发 "concurrent write to websocket connection" panic,
	// 进程崩溃、全队掉线。所有 WriteMessage 必须持本锁。
	mu   sync.Mutex
	send chan []byte // outbound to browser
}

// NewHub constructs a hub. `secret` is the token agents must present.
func NewHub(store *Store, secret string) *Hub {
	return &Hub{
		agents:  make(map[string]*agentConn),
		viewers: make(map[*viewerConn]struct{}),
		store:   store,
		secret:  []byte(secret),
	}
}

// HandleAgent runs the lifecycle for one agent connection: authenticates it,
// then reads upstream state messages and fans them out to browsers.
func (h *Hub) HandleAgent(ws *websocket.Conn) {
	// Require authentication within a short window so an unauthenticated
	// peer cannot hold a connection open indefinitely.
	ws.SetReadLimit(512 * 1024) // 512KB max per message
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))

	// First frame must be auth.
	var auth proto.Message
	if err := readMsg(ws, &auth); err != nil {
		log.Printf("agent auth read: %v", err)
		return
	}
	if auth.Type != proto.MsgAuth || auth.Auth == nil {
		h.reject(ws)
		return
	}
	// Constant-time comparison to avoid leaking the token via timing.
	if subtle.ConstantTimeCompare([]byte(auth.Auth.Token), h.secret) != 1 {
		h.reject(ws)
		return
	}
	// Bound the agent id/name so a peer can't send megabytes of data here.
	id, name := auth.Auth.AgentID, auth.Auth.Name
	if len(id) == 0 || len(id) > 128 || len(name) > 128 {
		h.reject(ws)
		return
	}

	ac := &agentConn{
		id:   id,
		name: name,
		ws:   ws,
	}
	// Send the auth result BEFORE registering / entering the read loop, so
	// the agent unblocks its handshake and starts streaming state.
	_ = writeMsg(ws, proto.Message{Type: proto.MsgAuthResult, AuthResult: &proto.AuthResult{OK: true}})
	// Authenticated from here on: no more read deadline.
	_ = ws.SetReadDeadline(time.Time{})
	// Each pong refreshes the read deadline so a silently-dropped
	// connection (agent host suspended, NAT entry expired) is detected
	// instead of lingering as a registered agent.
	ws.SetPongHandler(func(string) error {
		_ = ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})
	_ = ws.SetReadDeadline(time.Now().Add(90 * time.Second))

	h.mu.Lock()
	h.agents[ac.id] = ac
	h.mu.Unlock()
	h.store.RememberAgent(ac.id, ac.name)
	log.Printf("agent registered: %s (%s)", ac.name, ac.id)

	defer func() {
		h.mu.Lock()
		if cur := h.agents[ac.id]; cur == ac {
			delete(h.agents, ac.id)
		}
		h.mu.Unlock()
		ws.Close()
	}()

	// Ping the agent every 30s to refresh its read deadline via pong.
	go func() {
		ping := time.NewTicker(30 * time.Second)
		defer ping.Stop()
		for range ping.C {
			_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}()

	// Reader loop: the only message type we act on is state.
	for {
		var msg proto.Message
		if err := readMsg(ws, &msg); err != nil {
			log.Printf("agent %s read: %v", ac.id, err)
			return
		}
		if msg.Type == proto.MsgState && msg.State != nil {
			h.store.UpdateState(msg.State)
			h.broadcast(msg.State)
		}
	}
}

// reject sends a single generic failure and closes, without revealing why.
func (h *Hub) reject(ws *websocket.Conn) {
	_ = writeMsg(ws, proto.Message{Type: proto.MsgAuthResult, AuthResult: &proto.AuthResult{OK: false}})
	_ = ws.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.ClosePolicyViolation, ""))
	ws.Close()
}

// HandleViewer registers a browser websocket and streams live states to it.
// The browser never sends anything back; this is a push-only channel.
func (h *Hub) HandleViewer(ws *websocket.Conn) {
	ws.SetReadLimit(4096) // viewers only send tiny keepalive
	vc := &viewerConn{ws: ws, send: make(chan []byte, 64)}
	h.mu.Lock()
	h.viewers[vc] = struct{}{}
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.viewers, vc)
		h.mu.Unlock()
		close(vc.send)
		ws.Close()
	}()

	go func() {
		for b := range vc.send {
			// Set a write deadline so a stalled network (Wi-Fi silent
			// disconnect, NAT timeout) can't block this goroutine
			// forever. A failed write unblocks range and lets the
			// deferred cleanup close the connection.
			vc.mu.Lock()
			_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			err := ws.WriteMessage(websocket.TextMessage, b)
			vc.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()

	// Periodic ping keeps the viewer's read deadline fresh and probes
	// liveness even when there are no state updates to push.
	go func() {
		ping := time.NewTicker(30 * time.Second)
		defer ping.Stop()
		for range ping.C {
			vc.mu.Lock()
			_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			err := ws.WriteMessage(websocket.PingMessage, nil)
			vc.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()

	// Send a snapshot of current states immediately on connect.
	for _, st := range h.store.Latest() {
		b := marshalState(st)
		select {
		case vc.send <- b:
		default:
		}
	}

	// Keep reading only to detect disconnects; the browser sends nothing.
	for {
		if _, _, err := ws.ReadMessage(); err != nil {
			return
		}
	}
}

// broadcast pushes a state to every connected browser.
func (h *Hub) broadcast(st *proto.State) {
	b := marshalState(st)
	h.mu.RLock()
	defer h.mu.RUnlock()
	for vc := range h.viewers {
		select {
		case vc.send <- b:
		default: // drop slow viewer
		}
	}
}

// applyRename updates the cached state's name for an agent (so the next API
// poll / list render uses the new name) and pushes a refresh to browsers.
func (h *Hub) applyRename(id, name string) {
	h.mu.Lock()
	if st := h.agents[id]; st != nil {
		st.name = name
	}
	h.mu.Unlock()
	// SetLatestNameCopy atomically renames and returns a shallow copy we
	// can marshal without racing a concurrent UpdateState.
	if st := h.store.SetLatestNameCopy(id, name); st != nil {
		h.broadcast(st)
	}
}

// removeAgent drops an agent from the in-memory connection map (used after
// admin deletion so a lingering connection doesn't re-register it).
func (h *Hub) removeAgent(id string) {
	h.mu.Lock()
	delete(h.agents, id)
	h.mu.Unlock()
}

func readMsg(ws *websocket.Conn, m *proto.Message) error {
	_, b, err := ws.ReadMessage()
	if err != nil {
		return err
	}
	return json.Unmarshal(b, m)
}

func writeMsg(ws *websocket.Conn, m proto.Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return ws.WriteMessage(websocket.TextMessage, b)
}
