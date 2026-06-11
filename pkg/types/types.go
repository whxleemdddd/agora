package types

import "time"

// MessageType 消息类型
type MessageType string

const (
	MsgHeartbeat   MessageType = "heartbeat"
	MsgHandshake   MessageType = "handshake"
	MsgChat        MessageType = "chat"
	MsgTask        MessageType = "task"
	MsgTaskResult  MessageType = "task_result"
	MsgToolCall    MessageType = "tool_call"
	MsgToolResult  MessageType = "tool_result"
	MsgBye         MessageType = "bye"
	MsgError       MessageType = "error"
)

// Message 消息信封
type Message struct {
	Type      MessageType `json:"type"`
	From      string      `json:"from"`
	To        string      `json:"to"`
	ID        string      `json:"id"`
	Timestamp time.Time   `json:"timestamp"`
	Payload   interface{} `json:"payload,omitempty"`
}

// AgentCard Agent 身份卡片
type AgentCard struct {
	AgentID     string            `json:"agent_id"`
	Name        string            `json:"name"`
	Framework   string            `json:"framework"` // hermes, claude-code, custom
	Skills      []SkillDesc       `json:"skills"`
	Endpoint    string            `json:"endpoint"`  // ws://ip:port
	Status      AgentStatus       `json:"status"`
	Capabilities map[string]bool  `json:"capabilities,omitempty"`
}

type AgentStatus string

const (
	StatusOnline  AgentStatus = "online"
	StatusBusy    AgentStatus = "busy"
	StatusAway    AgentStatus = "away"
	StatusOffline AgentStatus = "offline"
)

type SkillDesc struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// PeerInfo Mesh 中的对端信息
type PeerInfo struct {
	Card    AgentCard `json:"card"`
	ConnID  string    `json:"conn_id"`
	LastSeen time.Time `json:"last_seen"`
}
