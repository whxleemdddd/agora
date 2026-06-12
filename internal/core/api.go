package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/whxleem/agora/pkg/types"
)
const APIPort = 7981

// Server Agora HTTP API + Web Dashboard
type Server struct {
	ag   *Agora
	mux  *http.ServeMux
	srv  *http.Server
}

func NewAPIServer(ag *Agora) *Server {
	s := &Server{ag: ag, mux: http.NewServeMux()}
	s.mux.HandleFunc("/api/status", s.handleStatus)
	s.mux.HandleFunc("/api/peers", s.handlePeers)
	s.mux.HandleFunc("/api/self", s.handleSelf)
	s.mux.HandleFunc("/api/localAgents", s.handleLocalAgents)
	s.mux.HandleFunc("/api/agents/register", s.handleAgentRegister)
	s.mux.HandleFunc("/api/agents/unregister", s.handleAgentUnregister)
	s.mux.HandleFunc("/api/agents/status", s.handleAgentStatus)
	s.mux.HandleFunc("/api/registeredAgents", s.handleRegisteredAgents)
	s.mux.HandleFunc("/api/tasks", s.handleTasks)
	s.mux.HandleFunc("/api/sendTask", s.handleSendTask)
	s.mux.HandleFunc("/api/connect", s.handleConnect)
	s.mux.HandleFunc("/api/broadcast", s.handleBroadcast)
	s.mux.HandleFunc("/api/messages", s.handleMessages)
	// Web Dashboard
	s.mux.Handle("/", http.FileServer(http.FS(webFS)))
	return s
}

func (s *Server) Start(ctx context.Context, port int) {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Printf("[api] listen error: %v", err)
		return
	}

	s.srv = &http.Server{
		Handler: s.mux,
		BaseContext: func(_ net.Listener) context.Context {
			return ctx
		},
	}

	go func() {
		if err := s.srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("[api] serve error: %v", err)
		}
	}()
	log.Printf("[api] dashboard: http://127.0.0.1:%d", port)
}

func (s *Server) Stop() {
	if s.srv != nil {
		s.srv.Close()
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"agent_id":   s.ag.Self().AgentID,
		"name":       s.ag.Self().Name,
		"status":     s.ag.Self().Status,
		"peers":      len(s.ag.Peers()),
		"api_port":   s.ag.apiPort,
		"mcp_port":   s.ag.mcpPort,
	})
}

func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.ag.Peers())
}

func (s *Server) handleSelf(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.ag.Self())
}

// handleConnect 手动连接一个对端（用于同机测试 / 跨网段场景）
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		Addr string `json:"addr"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	go s.ag.connectToPeer(req.Addr, "", req.Addr)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "connecting", "addr": req.Addr})
}

// handleBroadcast 通过 Hub 广播消息到 Mesh（或定向发送到指定对端）
func (s *Server) handleBroadcast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		Message string `json:"message"`
		To      string `json:"to,omitempty"` // 定向对端ID，空则广播
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}

	// 封装为标准的 Chat 消息
	msg := types.Message{
		Type:      types.MsgChat,
		From:      s.ag.self.AgentID,
		ID:        uuid.New().String()[:12],
		Timestamp: time.Now(),
		Payload:   req.Message,
	}

	// 如果指定了 To，定向发送；否则广播
	if req.To != "" {
		msg.To = req.To
		data, _ := json.Marshal(msg)
		err := s.ag.hub.SendTo(req.To, data)
		if err != nil {
			http.Error(w, fmt.Sprintf("send to %s failed: %v", req.To, err), 404)
			return
		}
	} else {
		data, _ := json.Marshal(msg)
		s.ag.hub.Broadcast(data)
	}

	// 同时存入本地消息记录
	entry := fmt.Sprintf("[%s] %s: %s",
		time.Now().Format("15:04:05"),	s.ag.self.AgentID[:min(8, len(s.ag.self.AgentID))], req.Message)
	s.ag.mu.Lock()
	s.ag.messages = append(s.ag.messages, entry)
	if len(s.ag.messages) > 100 {
		s.ag.messages = s.ag.messages[len(s.ag.messages)-100:]
	}
	s.ag.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "broadcasted", "message": req.Message})
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.ag.GetMessages())
}

// handleLocalAgents 返回本机挂载的所有子Agent
func (s *Server) handleLocalAgents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.ag.LocalAgents())
}

// handleSendTask 向远程 Agent 发送任务
func (s *Server) handleSendTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		To      string          `json:"to"`
		Skill   string          `json:"skill"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if req.To == "" || req.Skill == "" {
		http.Error(w, "to and skill required", 400)
		return
	}
	taskID := uuid.New().String()[:12]
	msg := types.Message{
		Type:      types.MsgTask,
		From:      s.ag.self.AgentID,
		To:        req.To,
		ID:        taskID,
		Timestamp: time.Now(),
		Payload: map[string]interface{}{
			"skill":   req.Skill,
			"payload": req.Payload,
		},
	}
	data, _ := json.Marshal(msg)
	if err := s.ag.hub.SendTo(req.To, data); err != nil {
		http.Error(w, fmt.Sprintf("send task failed: %v", err), 404)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "sent", "task_id": taskID})
}

// handleTasks 返回所有任务记录
func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.ag.Tasks())
}

// handleAgentRegister HTTP 方式注册一个子Agent
func (s *Server) handleAgentRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req types.AgentRegistration
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if req.Name == "" {
		http.Error(w, "name required", 400)
		return
	}
	ra := s.ag.RegisterAgent(req.Name, req.Type, req.Skills, nil)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ra)
}

// handleAgentUnregister 注销一个子Agent
func (s *Server) handleAgentUnregister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	s.ag.UnregisterAgent(req.ID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "unregistered"})
}

// handleAgentStatus 更新子Agent状态
func (s *Server) handleAgentStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	s.ag.UpdateAgentStatus(req.ID, types.AgentStatusValue(req.Status))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

// handleRegisteredAgents 返回本机所有已注册的子Agent（含详细信息）
func (s *Server) handleRegisteredAgents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.ag.RegisteredAgents())
}
