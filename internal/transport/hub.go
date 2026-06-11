package transport

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// Hub WebSocket 连接管理器
type Hub struct {
	upgrader   websocket.Upgrader
	mu         sync.RWMutex
	conns      map[string]*websocket.Conn
	onConn     func(peerID string)
	onMessage  func(peerID string, msg []byte) // 新增：收到消息回调
	server     *http.Server
	listener   net.Listener
}

func NewHub() *Hub {
	return &Hub{
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		conns: make(map[string]*websocket.Conn),
	}
}

// Start 启动 WebSocket 服务端
func (h *Hub) Start(ctx context.Context, port int, onConn func(peerID string)) {
	h.onConn = onConn

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", h.handleWS)

	h.server = &http.Server{
		Handler: mux,
		BaseContext: func(_ net.Listener) context.Context { return ctx },
	}

	var err error
	h.listener, err = net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Printf("[hub] listen error: %v", err)
		return
	}

	if err := h.server.Serve(h.listener); err != nil && err != http.ErrServerClosed {
		log.Printf("[hub] serve error: %v", err)
	}
}

// SetOnMessage 设置消息回调
func (h *Hub) SetOnMessage(fn func(peerID string, msg []byte)) {
	h.onMessage = fn
}

func (h *Hub) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[hub] upgrade error: %v", err)
		return
	}

	peerID := r.URL.Query().Get("agent_id")
	if peerID == "" {
		peerID = fmt.Sprintf("peer-%d", len(h.conns)+1)
	}

	h.mu.Lock()
	h.conns[peerID] = conn
	h.mu.Unlock()

	if h.onConn != nil {
		h.onConn(peerID)
	}

	go func() {
		defer func() {
			h.mu.Lock()
			delete(h.conns, peerID)
			h.mu.Unlock()
			conn.Close()
		}()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				break
			}
			if h.onMessage != nil {
				h.onMessage(peerID, msg)
			}
		}
	}()
}

// Connect 主动连接到对端的 WebSocket
func (h *Hub) Connect(addr string, agentID string) (*websocket.Conn, error) {
	u := fmt.Sprintf("ws://%s/ws?agent_id=%s", addr, agentID)
	conn, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		return nil, err
	}

	h.mu.Lock()
	h.conns[agentID] = conn
	h.mu.Unlock()

	// 主动连接后也开始读消息
	go func() {
		defer func() {
			h.mu.Lock()
			delete(h.conns, agentID)
			h.mu.Unlock()
			conn.Close()
		}()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				break
			}
			if h.onMessage != nil {
				h.onMessage(agentID, msg)
			}
		}
	}()

	return conn, nil
}

func (h *Hub) Broadcast(msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for id, conn := range h.conns {
		if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			log.Printf("[hub] write to %s: %v, closing", id, err)
			conn.Close()
			go func(pid string) {
				h.mu.Lock()
				delete(h.conns, pid)
				h.mu.Unlock()
			}(id)
		}
	}
}

func (h *Hub) SendTo(peerID string, msg []byte) error {
	h.mu.RLock()
	conn, ok := h.conns[peerID]
	h.mu.RUnlock()

	if !ok {
		return fmt.Errorf("peer %s not connected", peerID)
	}
	return conn.WriteMessage(websocket.TextMessage, msg)
}

func (h *Hub) Stop() {
	if h.server != nil {
		h.server.Close()
	}
}
