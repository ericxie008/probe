// Package proto defines the shared message types exchanged between the
// monitoring Agent and the Dashboard server over WebSocket.
package proto

// MsgType enumerates the messages flowing on the agent<->server channel.
type MsgType string

const (
	MsgAuth        MsgType = "auth"         // agent -> server: authenticate with a secret token
	MsgAuthResult  MsgType = "auth_result"  // server -> agent
	MsgState       MsgType = "state"        // agent -> server: periodic metrics
)

// Message is the envelope wrapping every exchanged payload.
type Message struct {
	Type MsgType `json:"type"`
	// Exactly one of the following is set, keyed off Type.
	Auth        *AuthPayload    `json:"auth,omitempty"`
	AuthResult  *AuthResult     `json:"auth_result,omitempty"`
	State       *State          `json:"state,omitempty"`
}

// AuthPayload is sent by the agent right after connecting.
type AuthPayload struct {
	Token   string `json:"token"`
	Name    string `json:"name"`
	AgentID string `json:"agent_id"`
}

// AuthResult tells the agent whether authentication succeeded.
type AuthResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// State is the periodic snapshot of host metrics.
type State struct {
	AgentID string  `json:"agent_id"`
	Name    string  `json:"name"`
	OS      string  `json:"os"`
	Arch    string  `json:"arch"`
	Uptime  uint64  `json:"uptime"`
	Load1   float64 `json:"load1"`
	Load5   float64 `json:"load5"`
	Load15  float64 `json:"load15"`

	CPUUsage    float64       `json:"cpu_usage"` // percent, 0-100
	CPUCount    int           `json:"cpu_count"`
	CPUModel    string        `json:"cpu_model"`
	CPUTemp     float64       `json:"cpu_temp"`
	CPUPerCore  []float64     `json:"cpu_per_core,omitempty"`

	MemoryTotal uint64 `json:"memory_total"`
	MemoryUsed  uint64 `json:"memory_used"`
	SwapTotal   uint64 `json:"swap_total"`
	SwapUsed    uint64 `json:"swap_used"`

	DiskTotal uint64         `json:"disk_total"`
	DiskUsed  uint64         `json:"disk_used"`
	Disks     []DiskInfo     `json:"disks,omitempty"`

	NetIn      uint64         `json:"net_in"`  // bytes since boot
	NetOut     uint64         `json:"net_out"` // bytes since boot
	NetSpeedIn uint64         `json:"net_speed_in"`  // bytes/s since last state
	NetSpeedOut uint64        `json:"net_speed_out"` // bytes/s since last state
	Interfaces []NetInterface `json:"interfaces,omitempty"`

	Processes  []ProcessInfo  `json:"processes,omitempty"`
	ConnCount  uint32         `json:"conn_count"` // established TCP connections
	BootTime   uint64         `json:"boot_time"`
	Timestamp  int64          `json:"timestamp"` // unix seconds
}

// DiskInfo describes a mounted filesystem.
type DiskInfo struct {
	Device     string `json:"device"`
	Mountpoint string `json:"mountpoint"`
	FSType     string `json:"fs_type"`
	Total      uint64 `json:"total"`
	Used       uint64 `json:"used"`
}

// NetInterface describes a network card.
type NetInterface struct {
	Name string `json:"name"`
	IPv4 string `json:"ipv4"`
	IPv6 string `json:"ipv6"`
	MAC  string `json:"mac"`
}

// ProcessInfo is a top-N process row.
type ProcessInfo struct {
	PID    int32   `json:"pid"`
	Name   string  `json:"name"`
	CPU    float64 `json:"cpu"`
	Memory uint64  `json:"memory"`
}

