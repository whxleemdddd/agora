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

	// Agent 注册协议
	MsgAgentRegister   MessageType = "agent_register"
	MsgAgentUnregister MessageType = "agent_unregister"
	MsgAgentStatus     MessageType = "agent_status" // 子Agent状态变更上报
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

// LocalAgent 本机挂载的一个子Agent（一个Agora实例管多个本机Agent）
type LocalAgent struct {
	ID         string            `json:"id" yaml:"id"`
	Name       string            `json:"name" yaml:"name"`
	Type       string            `json:"type" yaml:"type"`          // hermes, claude-code, custom
	Skills     []SkillDesc       `json:"skills" yaml:"skills"`
	Status     AgentStatus       `json:"status" yaml:"-"`
}

// PeerInfo Mesh 中的对端信息
type PeerInfo struct {
	Card      AgentCard     `json:"card"`
	ConnID    string        `json:"conn_id"`
	LastSeen  time.Time     `json:"last_seen"`
	SubAgents []LocalAgent  `json:"sub_agents,omitempty"` // 对端携带的子Agent列表
}

// Agent 注册状态
type AgentStatusValue string

const (
	AgentStatusIdle  AgentStatusValue = "idle"
	AgentStatusBusy  AgentStatusValue = "busy"
	AgentStatusAway  AgentStatusValue = "away"
	AgentStatusOffline AgentStatusValue = "offline"
)

// AgentRegistration 子Agent注册请求（通过 HTTP 或 WebSocket 提交）
type AgentRegistration struct {
	Name    string      `json:"name"`
	Type    string      `json:"type"`    // hermes, python, mcp, custom
	Skills  []SkillDesc `json:"skills"`
	Status  AgentStatusValue `json:"status"`
	ConnURL string     `json:"conn_url,omitempty"` // Agora 用来连接子Agent的地址（可选）
}

// RegisteredAgent 运行时已注册的子Agent
type RegisteredAgent struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Type      string            `json:"type"`
	Skills    []SkillDesc       `json:"skills"`
	Status    AgentStatusValue  `json:"status"`
	Conn      interface{}       `json:"-"` // 底层连接（WebSocket conn 或 MCP 客户端）
	Connected bool              `json:"connected"`
	LastSeen  time.Time         `json:"last_seen"`
}
