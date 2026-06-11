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
	AgentID   string
	Name      string
	Port      int
	Plugins   []string
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
	apiServer *Server
}

func New(cfg Config) *Agora {
	port := cfg.Port
	if port == 0 {
		port = discovery.ServicePort
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
	go ag.hub.Start(ctx, ag.cfg.Port, ag.handleIncomingConn)

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

	// 8. 启动 HTTP API
	apiSrv := NewAPIServer(ag)
	apiSrv.Start(ctx)
	ag.apiServer = apiSrv

	log.Printf("[agora] started — agent=%s, listening on :%d, API on :%d", ag.self.AgentID, ag.cfg.Port, APIPort)
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

func (ag *Agora) handleIncomingConn(peerID string) {
	log.Printf("[agora] incoming connection: %s", peerID)
	// 建一个临时的 peer 记录，等收到握手消息后更新
	ag.mu.Lock()
	ag.peers[peerID] = &types.PeerInfo{
		ConnID:    peerID,
		LastSeen:  time.Now(),
	}
	ag.mu.Unlock()
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
