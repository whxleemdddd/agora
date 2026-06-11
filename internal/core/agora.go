package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/whxleem/agora/internal/discovery"
	"github.com/whxleem/agora/internal/message"
	"github.com/whxleem/agora/internal/plugin"
	"github.com/whxleem/agora/internal/transport"
	"github.com/whxleem/agora/pkg/types"
)

// Config Agora 核心配置
type Config struct {
	AgentID    string
	Name       string
	Port       int
	APIPort    int
	MCPPort    int
	Plugins    []string
	ConfigFile string
}

// Agora 核心守护进程
type Agora struct {
	cfg       Config
	self      types.AgentCard
	mu        sync.RWMutex
	peers     map[string]*types.PeerInfo
	disco     *discovery.Discoverer
	hub       *transport.Hub
	msgRouter *message.Router
	plugins   []plugin.Plugin
	msgCh     chan []byte
	cancel    context.CancelFunc
	apiPort   int
	mcpPort   int
	apiServer *Server
	mcpBridge *MCPBridge
	skills    *SkillRegistry
	tasks     *TaskManager
}

func New(cfg Config) *Agora {
	port := cfg.Port
	if port == 0 {
		port = discovery.ServicePort
	}
	apiPort := cfg.APIPort
	if apiPort == 0 {
		apiPort = APIPort
	}
	mcpPort := cfg.MCPPort
	if mcpPort == 0 {
		mcpPort = MCPBridgePort
	}
	agentID := cfg.AgentID
	if agentID == "" {
		agentID = uuid.New().String()[:8]
	}
	name := cfg.Name
	if name == "" {
		name = "agora-" + agentID
	}

	return &Agora{
		cfg:   cfg,
		peers: make(map[string]*types.PeerInfo),
		self: types.AgentCard{
			AgentID:   agentID,
			Name:      name,
			Framework: "agora-core",
			Status:    types.StatusOnline,
			Endpoint:  fmt.Sprintf("ws://0.0.0.0:%d", port),
		},
		disco:     discovery.NewDiscoverer(),
		hub:       transport.NewHub(),
		msgRouter: message.NewRouter(),
		msgCh:     make(chan []byte, 64),
		skills:    NewSkillRegistry(),
		tasks:     NewTaskManager(),
		apiPort:   apiPort,
		mcpPort:   mcpPort,
	}
}

// Start 启动 Agora 核心
func (ag *Agora) Start(ctx context.Context) error {
	ctx, ag.cancel = context.WithCancel(ctx)

	// 1. 启动 mDNS 发现
	log.Printf("[agora] starting mDNS discovery (agent=%s, name=%s)", ag.self.AgentID, ag.self.Name)
	if err := ag.disco.Start(ctx, ag.self.AgentID, ag.self.Name); err != nil {
		return fmt.Errorf("mDNS start: %w", err)
	}

	// 2. 启动 WebSocket Hub
	log.Printf("[agora] starting WebSocket hub on port %d", ag.cfg.Port)
	go ag.hub.Start(ctx, ag.cfg.Port, ag.onPeerConn)
	ag.hub.SetOnMessage(ag.handleMessage)

	// 3. 启动消息路由
	go ag.msgRouter.Start(ctx)

	// 4. 加载并启动插件
	ag.loadPlugins(ctx)

	// 5. 消息处理主循环
	go ag.messageLoop(ctx)

	// 6. 监听 mDNS 发现事件
	go ag.discoveryLoop(ctx)

	// 7. 心跳发送器
	go ag.heartbeatLoop(ctx)

	// 8. 保存首次配置（自动生成 Agent ID 后写入）
	if ag.cfg.ConfigFile != "" {
		appCfg := DefaultConfig()
		appCfg.Agent.ID = ag.self.AgentID
		appCfg.Agent.Name = ag.self.Name
		appCfg.Mesh.Port = ag.cfg.Port
		appCfg.API.Port = ag.cfg.APIPort
		if appCfg.API.Port == 0 {
			appCfg.API.Port = APIPort
		}
		appCfg.API.MCP = ag.cfg.MCPPort
		if appCfg.API.MCP == 0 {
			appCfg.API.MCP = MCPBridgePort
		}
		if err := SaveConfig(appCfg, ag.cfg.ConfigFile); err != nil {
			log.Printf("[agora] save config: %v", err)
		}
	}

	// 9. 启动 HTTP API
	apiSrv := NewAPIServer(ag)
	apiSrv.Start(ctx, ag.apiPort)
	ag.apiServer = apiSrv

	// 10. 启动 MCP Bridge
	mcpBridge := NewMCPBridge(ag)
	mcpBridge.Start(ctx, ag.mcpPort)
	ag.mcpBridge = mcpBridge

	log.Printf("[agora] started — agent=%s, WS on :%d, API on :%d, MCP on :%d",
		ag.self.AgentID, ag.cfg.Port, ag.apiPort, ag.mcpPort)
	return nil
}

// ── 插件加载 ──────────────────────────────────────────

func (ag *Agora) loadPlugins(ctx context.Context) {
	wanted := ag.cfg.Plugins
	if len(wanted) == 0 {
		wanted = plugin.GetRegistered()
	}
	for _, name := range wanted {
		p, ok := plugin.New(name)
		if !ok {
			log.Printf("[agora] plugin %s not registered, skipping", name)
			continue
		}
		if !p.Detect(ctx) {
			continue
		}
		log.Printf("[agora] plugin %s: detected, connecting...", name)
		if err := p.Connect(ctx); err != nil {
			log.Printf("[agora] plugin %s: connect failed: %v", name, err)
			continue
		}
		stop, err := p.ListenAgent(ctx, ag.msgCh)
		if err != nil {
			log.Printf("[agora] plugin %s: listen failed: %v", name, err)
			continue
		}
		_ = stop
		ag.plugins = append(ag.plugins, p)
		log.Printf("[agora] plugin %s: connected and listening", name)
	}
}

// ── mDNS 发现 → 自动 WS 连接 ─────────────────────────

func (ag *Agora) discoveryLoop(ctx context.Context) {
	// 避免连接自己
	localIP := getOutboundIP()
	seen := make(map[string]bool) // "ip:port" 去重

	for {
		select {
		case entry := <-ag.disco.Entries():
			peerIP := entry.AddrV4.String()
			peerPort := entry.Port
			peerKey := net.JoinHostPort(peerIP, strconv.Itoa(peerPort))

			// 跳过自己
			if peerIP == localIP && peerPort == ag.cfg.Port {
				continue
			}
			if seen[peerKey] {
				continue
			}
			seen[peerKey] = true

			// 从 TXT record 提取 peer 的名字
			peerName := peerIP
			for _, txt := range entry.InfoFields {
				if strings.HasPrefix(txt, "name=") {
					peerName = strings.TrimPrefix(txt, "name=")
					break
				}
			}

			log.Printf("[agora] discovered peer: %s @ %s", peerName, peerKey)

			// 主动连接对端
			agentID := strings.TrimSuffix(entry.Name, "."+discovery.ServiceName)
			go ag.connectToPeer(peerKey, agentID, peerName)

		case <-ctx.Done():
			return
		}
	}
}

func (ag *Agora) connectToPeer(addr, agentID, name string) {
	conn, err := ag.hub.Connect(addr, agentID)
	if err != nil {
		log.Printf("[agora] connect to %s (%s) failed: %v", name, addr, err)
		return
	}
	_ = conn

	// 握手：发送自己的 AgentCard
	cardData, _ := json.Marshal(ag.self)
	handshake := types.Message{
		Type:      types.MsgHandshake,
		From:      ag.self.AgentID,
		To:        agentID,
		ID:        uuid.New().String()[:12],
		Timestamp: time.Now(),
		Payload:   string(cardData),
	}
	msgData, _ := json.Marshal(handshake)
	ag.hub.SendTo(agentID, msgData)

	log.Printf("[agora] handshake sent to %s (%s)", name, agentID)
}

// ── 入站连接处理 ──────────────────────────────────────

func (ag *Agora) onPeerConn(peerID string) {
	log.Printf("[agora] peer connected: %s", peerID)
	ag.mu.Lock()
	ag.peers[peerID] = &types.PeerInfo{
		ConnID:    peerID,
		LastSeen:  time.Now(),
	}
	ag.mu.Unlock()

	// 回发握手消息
	cardData, _ := json.Marshal(ag.self)
	handshake := types.Message{
		Type:      types.MsgHandshake,
		From:      ag.self.AgentID,
		To:        peerID,
		ID:        uuid.New().String()[:12],
		Timestamp: time.Now(),
		Payload:   string(cardData),
	}
	msgData, _ := json.Marshal(handshake)
	ag.hub.SendTo(peerID, msgData)
	log.Printf("[agora] handshake sent back to %s", peerID)
}

// handleMessage 处理从 Hub 收到的消息
func (ag *Agora) handleMessage(peerID string, data []byte) {
	var msg types.Message
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}

	switch msg.Type {
	case types.MsgHandshake:
		// 收到握手消息，更新对端的 AgentCard
		cardStr, ok := msg.Payload.(string)
		if !ok {
			return
		}
		var card types.AgentCard
		if err := json.Unmarshal([]byte(cardStr), &card); err != nil {
			return
		}

		ag.mu.Lock()
		if peer, exists := ag.peers[msg.From]; exists {
			peer.Card = card
			peer.LastSeen = time.Now()
		} else {
			ag.peers[msg.From] = &types.PeerInfo{
				Card:     card,
				ConnID:   msg.From,
				LastSeen: time.Now(),
			}
		}
		ag.mu.Unlock()

		// 注册对端技能
		ag.skills.RegisterRemote(msg.From, card.Skills)

		log.Printf("[agora] handshake received from %s (%s)", card.Name, msg.From)

	case types.MsgHeartbeat:
		// 收到心跳，更新最后在线时间
		ag.mu.Lock()
		if peer, exists := ag.peers[msg.From]; exists {
			peer.LastSeen = time.Now()
		}
		ag.mu.Unlock()

	case types.MsgChat:
		log.Printf("[agora] chat from %s: %v", msg.From, msg.Payload)

	case types.MsgTask:
		ag.handleTask(context.Background(), msg)
	}
}

// ── 心跳 ──────────────────────────────────────────────

func (ag *Agora) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			msg := types.Message{
				Type:      types.MsgHeartbeat,
				From:      ag.self.AgentID,
				Timestamp: time.Now(),
			}
			data, _ := json.Marshal(msg)
			ag.hub.Broadcast(data)

			// 清理超过 90 秒未心跳的对端
			ag.mu.Lock()
			now := time.Now()
			for id, peer := range ag.peers {
				if now.Sub(peer.LastSeen) > 90*time.Second {
					log.Printf("[agora] peer %s timed out, removing", id)
					delete(ag.peers, id)
				}
			}
			ag.mu.Unlock()

		case <-ctx.Done():
			return
		}
	}
}

// ── 消息循环 ──────────────────────────────────────────

func (ag *Agora) messageLoop(ctx context.Context) {
	for {
		select {
		case msg := <-ag.msgCh:
			ag.hub.Broadcast(msg)
		case <-ctx.Done():
			return
		}
	}
}

// ── 对外接口 ──────────────────────────────────────────

// Peers 返回当前在线的对端列表
func (ag *Agora) Peers() []types.PeerInfo {
	ag.mu.RLock()
	defer ag.mu.RUnlock()
	list := make([]types.PeerInfo, 0, len(ag.peers))
	for _, p := range ag.peers {
		list = append(list, *p)
	}
	return list
}

// Self 返回自身 AgentCard
func (ag *Agora) Self() types.AgentCard {
	return ag.self
}

// ── 停止 ──────────────────────────────────────────────

func (ag *Agora) Stop() {
	ag.cancel()
	ag.disco.Stop()
	ag.hub.Stop()
	if ag.apiServer != nil {
		ag.apiServer.Stop()
	}
	if ag.mcpBridge != nil {
		ag.mcpBridge.Stop()
	}
	for _, p := range ag.plugins {
		p.Close()
	}
	log.Print("[agora] stopped")
}

// ── 工具函数 ─────────────────────────────────────────

func getOutboundIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	return strings.Split(conn.LocalAddr().String(), ":")[0]
}
