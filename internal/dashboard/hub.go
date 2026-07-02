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
	Type string       `json:"type"`
	Data *proto.State `json:"data"`
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
	ws   *websocket.Conn
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

	// Reader loop: the only message type we act on is state.
	for {
		// Reset a generous read deadline each iteration; the agent sends
		// state every few seconds, so 5 minutes of silence means it's dead.
		_ = ws.SetReadDeadline(time.Now().Add(5 * time.Minute))
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
			if err := ws.WriteMessage(websocket.TextMessage, b); err != nil {
				return
			}
		}
	}()

	// Send a snapshot of current states immediately on connect.
	for _, st := range h.store.Latest() {
		b, _ := json.Marshal(stateMsg{Type: "state", Data: st})
		select {
		case vc.send <- b:
		default:
		}
	}

	// Keep reading only to detect disconnects; the browser sends nothing.
	for {
		_ = ws.SetReadDeadline(time.Now().Add(2 * time.Minute))
		if _, _, err := ws.ReadMessage(); err != nil {
			return
		}
	}
}

// broadcast pushes a state to every connected browser.
func (h *Hub) broadcast(st *proto.State) {
	b, _ := json.Marshal(stateMsg{Type: "state", Data: st})
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
	h.store.SetLatestName(id, name)
	if st := h.store.LatestByID(id); st != nil {
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
