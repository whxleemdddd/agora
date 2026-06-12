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
	messages  []string

	// 动态注册的子Agent（运行时从配置加载 + HTTP/WS 注册）
	registeredAgents map[string]*types.RegisteredAgent
	agentsMu         sync.RWMutex
	agentIDCounter   int
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

	ag := &Agora{
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
		messages:  make([]string, 0, 100),
		registeredAgents: make(map[string]*types.RegisteredAgent),
	}

	// 从配置文件加载静态声明的子Agent作为初始注册
	ag.loadLocalAgentsFromConfig()

	return ag
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

	// 8. 保存首次配置（自动生成 Agent ID 后写入，保留原有 agents/peers）
	if ag.cfg.ConfigFile != "" {
		// 先读现有配置，保留 agents 和 peers
		existingCfg, _ := LoadConfig(ag.cfg.ConfigFile)
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
		// 保留用户配置的 agents 和 peers
		if len(existingCfg.Agents) > 0 {
			appCfg.Agents = existingCfg.Agents
		}
		if len(existingCfg.Peers) > 0 {
			appCfg.Peers = existingCfg.Peers
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

		// 注册插件技能
		for _, skill := range p.Skills() {
			ag.skills.RegisterLocal(skill)
		}
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
	connAgentID := agentID
	if connAgentID == "" {
		connAgentID = fmt.Sprintf("conn-%s", uuid.New().String()[:8])
	}
	conn, err := ag.hub.Connect(addr, connAgentID)
	if err != nil {
		log.Printf("[agora] connect to %s (%s) failed: %v", name, addr, err)
		return
	}
	_ = conn

	// 握手：发送自己的 AgentCard + 子Agent列表
	toID := agentID
	if toID == "" {
		toID = connAgentID
	}
	handshakePayload, _ := json.Marshal(map[string]interface{}{
		"card":       ag.self,
		"sub_agents": ag.LocalAgents(),
	})
	handshake := types.Message{
		Type:      types.MsgHandshake,
		From:      ag.self.AgentID,
		To:        toID,
		ID:        uuid.New().String()[:12],
		Timestamp: time.Now(),
		Payload:   string(handshakePayload),
	}
	msgData, _ := json.Marshal(handshake)
	ag.hub.SendTo(connAgentID, msgData)

	log.Printf("[agora] handshake sent to %s (%s) with %d sub-agents", name, agentID, len(ag.RegisteredAgents()))
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

	// 回发握手消息（含子Agent列表）
	handshakePayload, _ := json.Marshal(map[string]interface{}{
		"card":       ag.self,
		"sub_agents": ag.LocalAgents(),
	})
	handshake := types.Message{
		Type:      types.MsgHandshake,
		From:      ag.self.AgentID,
		To:        peerID,
		ID:        uuid.New().String()[:12],
		Timestamp: time.Now(),
		Payload:   string(handshakePayload),
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
		// 收到握手消息，更新对端的 AgentCard 和子Agent列表
		payloadStr, ok := msg.Payload.(string)
		if !ok {
			return
		}
		var handshakeData struct {
			Card      types.AgentCard    `json:"card"`
			SubAgents []types.LocalAgent `json:"sub_agents"`
		}
		if err := json.Unmarshal([]byte(payloadStr), &handshakeData); err != nil {
			return
		}
		card := handshakeData.Card
		subs := handshakeData.SubAgents
		if subs == nil {
			subs = []types.LocalAgent{}
		}

		ag.mu.Lock()
		if peer, exists := ag.peers[msg.From]; exists {
			peer.Card = card
			peer.SubAgents = subs
			peer.LastSeen = time.Now()
		} else {
			ag.peers[msg.From] = &types.PeerInfo{
				Card:      card,
				ConnID:    msg.From,
				LastSeen:  time.Now(),
				SubAgents: subs,
			}
		}
		ag.mu.Unlock()

		// 注册对端技能 + 子Agent技能
		ag.skills.RegisterRemote(msg.From, card.Skills)
		for _, s := range subs {
			ag.skills.RegisterRemote(msg.From, s.Skills)
		}

		// 如果握手消息的 From 与关联的 connID 不同，更新 Hub 的 conn key
		// （处理手动连接时临时ID -> 真实agentID 的转换）
		if msg.From != "" {
			ag.mu.RLock()
			for _, peer := range ag.peers {
				if peer.ConnID != msg.From && peer.Card.AgentID == msg.From {
					ag.hub.UpdateConnID(peer.ConnID, msg.From)
					peer.ConnID = msg.From
				}
			}
			ag.mu.RUnlock()
			// 如果没有匹配到 peer，也尝试直接更新（临时ID的情况）
			ag.hub.UpdateConnID(peerID, msg.From)
		}

		log.Printf("[agora] handshake received from %s (%s) with %d sub-agents", card.Name, msg.From, len(subs))

	case types.MsgHeartbeat:
		// 收到心跳，更新最后在线时间
		ag.mu.Lock()
		if peer, exists := ag.peers[msg.From]; exists {
			peer.LastSeen = time.Now()
		}
		ag.mu.Unlock()

		case types.MsgChat:
			payloadStr := fmt.Sprintf("%v", msg.Payload)

			// 定向私聊：如果 To 指定了本机，只处理不广播
			if msg.To != "" && msg.To != ag.self.AgentID {
				// 消息不是发给本机的，忽略
				return
			}

			entry := fmt.Sprintf("[%s] %s ➤ %s: %s",
				time.Now().Format("15:04:05"), msg.From, msg.To, payloadStr)
			ag.mu.Lock()
			ag.messages = append(ag.messages, entry)
			if len(ag.messages) > 100 {
				ag.messages = ag.messages[len(ag.messages)-100:]
			}
			ag.mu.Unlock()
			log.Printf("[agora] %s", entry)

			// 转发消息给本机插件处理，并期待回复
			for _, p := range ag.plugins {
				// 把原始消息的 From 写入上下文，供插件回复使用
				replyMsg := struct {
					types.Message
					ReplyTo string `json:"reply_to"`
				}{
					Message: msg,
					ReplyTo: msg.From,
				}
				enriched, _ := json.Marshal(replyMsg)
				if err := p.SendToAgent(context.Background(), enriched); err != nil {
					log.Printf("[agora] send to plugin %s: %v", p.Name(), err)
				}
			}

	case types.MsgTask:
		ag.handleTask(context.Background(), msg)

	case types.MsgTaskResult:
		// 收到任务结果，更新任务记录并记录消息
		resultStr := fmt.Sprintf("%v", msg.Payload)
		ag.tasks.Complete(msg.ID, []byte(resultStr))
		entry := fmt.Sprintf("[%s] ➤ 任务完成: %s → %s",
			time.Now().Format("15:04:05"), msg.From, resultStr)
		ag.mu.Lock()
		ag.messages = append(ag.messages, entry)
		ag.mu.Unlock()
		log.Printf("[agora] %s", entry)
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
			// 尝试解析
			var parsed types.Message
			if err := json.Unmarshal(msg, &parsed); err != nil || parsed.Type == "" {
				// 原始文本，尝试从 embedded JSON 提取 reply_to
				var embedded struct {
					ReplyTo string `json:"reply_to"`
				}
				_ = json.Unmarshal(msg, &embedded)
				parsed = types.Message{
					Type:      types.MsgChat,
					From:      ag.self.AgentID,
					To:        embedded.ReplyTo,
					ID:        uuid.New().String()[:12],
					Timestamp: time.Now(),
					Payload:   string(msg),
				}
			}

			if parsed.To != "" && parsed.To != ag.self.AgentID {
				// 定向回复
				data, _ := json.Marshal(parsed)
				ag.hub.SendTo(parsed.To, data)
			} else {
				// 广播
				data, _ := json.Marshal(parsed)
				ag.hub.Broadcast(data)
			}
		case <-ctx.Done():
			return
		}
	}
}

// ── 对外接口 ──────────────────────────────────────────

// RegisteredAgents 返回本机所有已注册的子Agent
func (ag *Agora) RegisteredAgents() []types.RegisteredAgent {
	ag.agentsMu.RLock()
	defer ag.agentsMu.RUnlock()
	list := make([]types.RegisteredAgent, 0, len(ag.registeredAgents))
	for _, ra := range ag.registeredAgents {
		list = append(list, *ra)
	}
	return list
}

// RegisterAgent 注册一个子Agent（来自 HTTP API 或 WebSocket 注册）
func (ag *Agora) RegisterAgent(name, agentType string, skills []types.SkillDesc, conn interface{}) *types.RegisteredAgent {
	ag.agentsMu.Lock()
	defer ag.agentsMu.Unlock()
	ag.agentIDCounter++
	id := fmt.Sprintf("%s-%s-%d", ag.self.AgentID, name, ag.agentIDCounter)

	ra := &types.RegisteredAgent{
		ID:        id,
		Name:      name,
		Type:      agentType,
		Skills:    skills,
		Status:    types.AgentStatusIdle,
		Conn:      conn,
		Connected: true,
		LastSeen:  time.Now(),
	}
	ag.registeredAgents[id] = ra

	// 注册技能到全局 SkillRegistry
	for _, sk := range skills {
		ag.skills.RegisterLocal(sk)
	}

	// 向 Mesh 广播 Agent 上线（握手协议携带更新后的子Agent列表）
	go ag.broadcastAgentList()

	log.Printf("[agora] agent registered: %s (type=%s, skills=%d)", name, agentType, len(skills))
	return ra
}

// UnregisterAgent 注销一个子Agent
func (ag *Agora) UnregisterAgent(id string) {
	ag.agentsMu.Lock()
	delete(ag.registeredAgents, id)
	ag.agentsMu.Unlock()

	go ag.broadcastAgentList()
	log.Printf("[agora] agent unregistered: %s", id)
}

// UpdateAgentStatus 更新子Agent状态
func (ag *Agora) UpdateAgentStatus(id string, status types.AgentStatusValue) {
	ag.agentsMu.Lock()
	if ra, ok := ag.registeredAgents[id]; ok {
		ra.Status = status
		ra.LastSeen = time.Now()
	}
	ag.agentsMu.Unlock()
}

// broadcastAgentList 向 Mesh 广播本机子Agent列表更新
func (ag *Agora) broadcastAgentList() {
	agents := ag.RegisteredAgents()
	payload, _ := json.Marshal(map[string]interface{}{
		"card":       ag.self,
		"sub_agents": agents,
	})
	msg := types.Message{
		Type:      types.MsgHandshake,
		From:      ag.self.AgentID,
		ID:        uuid.New().String()[:12],
		Timestamp: time.Now(),
		Payload:   string(payload),
	}
	data, _ := json.Marshal(msg)
	ag.hub.Broadcast(data)
}

// LocalAgents 兼容旧接口，返回已注册子Agent的 LocalAgent 视图
func (ag *Agora) LocalAgents() []types.LocalAgent {
	ag.agentsMu.RLock()
	defer ag.agentsMu.RUnlock()
	list := make([]types.LocalAgent, 0, len(ag.registeredAgents))
	for _, ra := range ag.registeredAgents {
		list = append(list, types.LocalAgent{
			ID:     ra.ID,
			Name:   ra.Name,
			Type:   ra.Type,
			Status: types.AgentStatus(ra.Status),
			Skills: ra.Skills,
		})
	}
	return list
}

// loadLocalAgentsFromConfig 从配置文件中加载子Agent声明作为初始注册
func (ag *Agora) loadLocalAgentsFromConfig() {
	if ag.cfg.ConfigFile == "" {
		return
	}
	appCfg, err := LoadConfig(ag.cfg.ConfigFile)
	if err != nil {
		return
	}
	if len(appCfg.Agents) == 0 {
		return
	}
	ag.agentsMu.Lock()
	defer ag.agentsMu.Unlock()
	for i, entry := range appCfg.Agents {
		id := fmt.Sprintf("%s-%s", ag.self.AgentID, entry.Name)
		ra := &types.RegisteredAgent{
			ID:        id,
			Name:      entry.Name,
			Type:      entry.Type,
			Status:    types.AgentStatusIdle,
			Connected: true,
			LastSeen:  time.Now(),
		}
		if entry.Skills != nil {
			ra.Skills = entry.Skills
		}
		ag.registeredAgents[id] = ra
		log.Printf("[agora] local agent loaded: %s (type=%s, skills=%d)", entry.Name, entry.Type, len(ra.Skills))

		// 同时将子Agent的技能注册到全局SkillRegistry
		for _, sk := range ra.Skills {
			ag.skills.RegisterLocal(sk)
		}
		_ = i
	}
}

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

// GetMessages 返回消息历史
func (ag *Agora) GetMessages() []string {
	ag.mu.RLock()
	defer ag.mu.RUnlock()
	cp := make([]string, len(ag.messages))
	copy(cp, ag.messages)
	return cp
}

// Tasks 返回任务记录列表
func (ag *Agora) Tasks() []TaskRecord {
	return ag.tasks.List()
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
