package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/whxleem/agora/pkg/types"
)

// MCPBridge 把 Mesh 上的 Agent 技能暴露为 MCP tools
// 任何 MCP 客户端（Claude Code、Hermes 等）可以通过标准的 MCP 协议调用
type MCPBridge struct {
	ag     *Agora
	mux    *http.ServeMux
	srv    *http.Server
	port   int
}

const MCPBridgePort = 7982

// MCP JSON-RPC 请求/响应
type MCPRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type MCPResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *MCPError   `json:"error,omitempty"`
}

type MCPError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type MCPTool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"inputSchema"`
}

func NewMCPBridge(ag *Agora) *MCPBridge {
	m := &MCPBridge{ag: ag, port: MCPBridgePort, mux: http.NewServeMux()}
	m.mux.HandleFunc("/mcp", m.handleMCP)
	return m
}

func (mb *MCPBridge) Start(ctx context.Context) {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", mb.port))
	if err != nil {
		log.Printf("[mcp] listen error: %v", err)
		return
	}
	mb.srv = &http.Server{
		Handler: mb.mux,
		BaseContext: func(_ net.Listener) context.Context { return ctx },
	}
	go func() {
		if err := mb.srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("[mcp] serve error: %v", err)
		}
	}()
	log.Printf("[mcp] MCP Bridge listening on :%d", mb.port)

	// 注册本机内置技能
	mb.ag.skills.RegisterLocal(types.SkillDesc{
		Name:        "agora_broadcast",
		Description: "向 Mesh 中所有 Agent 广播消息",
	})
	mb.ag.skills.RegisterLocal(types.SkillDesc{
		Name:        "agora_find_agent",
		Description: "按技能名称查找 Mesh 中能处理该技能的 Agent",
	})
}

func (mb *MCPBridge) Stop() {
	if mb.srv != nil {
		mb.srv.Close()
	}
}

func (mb *MCPBridge) handleMCP(w http.ResponseWriter, r *http.Request) {
	var req MCPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(MCPResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &MCPError{Code: -32700, Message: "Parse error"},
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")

	switch req.Method {
	case "tools/list":
		mb.handleListTools(w, req)
	case "tools/call":
		mb.handleCallTool(w, req, r.Context())
	default:
		json.NewEncoder(w).Encode(MCPResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &MCPError{Code: -32601, Message: "Method not found"},
		})
	}
}

func (mb *MCPBridge) handleListTools(w http.ResponseWriter, req MCPRequest) {
	tools := []MCPTool{
		{
			Name:        "agora_broadcast",
			Description: "向 Mesh 中所有在线 Agent 广播消息",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"message": map[string]interface{}{
						"type":        "string",
						"description": "要广播的消息内容",
					},
				},
				"required": []string{"message"},
			},
		},
		{
			Name:        "agora_find_agent",
			Description: "按技能名称查找能处理该技能的远程 Agent",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"skill": map[string]interface{}{
						"type":        "string",
						"description": "技能名称",
					},
				},
				"required": []string{"skill"},
			},
		},
	}

	json.NewEncoder(w).Encode(MCPResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]interface{}{
			"tools": tools,
		},
	})
}

func (mb *MCPBridge) handleCallTool(w http.ResponseWriter, req MCPRequest, ctx context.Context) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	json.Unmarshal(req.Params, &params)

	switch params.Name {
	case "agora_broadcast":
		var args struct {
			Message string `json:"message"`
		}
		json.Unmarshal(params.Arguments, &args)

		msg := types.Message{
			Type:      types.MsgChat,
			From:      mb.ag.self.AgentID,
			ID:        "mcp-" + fmt.Sprintf("%d", req.ID),
			Timestamp: time.Now(),
			Payload:   args.Message,
		}
		data, _ := json.Marshal(msg)
		mb.ag.hub.Broadcast(data)

		json.NewEncoder(w).Encode(MCPResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]string{"status": "broadcasted"},
		})

	case "agora_find_agent":
		var args struct {
			Skill string `json:"skill"`
		}
		json.Unmarshal(params.Arguments, &args)
		agents := mb.ag.skills.FindRemote(args.Skill)

		json.NewEncoder(w).Encode(MCPResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]interface{}{"agents": agents},
		})

	default:
		json.NewEncoder(w).Encode(MCPResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &MCPError{Code: -32602, Message: "Unknown tool: " + params.Name},
		})
	}
}
