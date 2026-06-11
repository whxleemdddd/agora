package core

import (
	"context"
	"fmt"
	"log"
	"sync"

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
	Plugins   []string // 启用哪些插件，空数组=全部
}

// Agora 核心守护进程
type Agora struct {
	cfg       Config
	self      types.AgentCard
	mu        sync.RWMutex
	peers     map[string]*types.PeerInfo // agentID -> peer
	disco     *discovery.Discoverer
	hub       *transport.Hub
	msgRouter *message.Router
	plugins   []plugin.Plugin
	msgCh     chan []byte // agent -> mesh
	cancel    context.CancelFunc
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
	go ag.hub.Start(ctx, ag.cfg.Port, func(peerID string) {
		ag.onPeerConn(peerID)
	})

	// 3. 启动消息路由
	go ag.msgRouter.Start(ctx)

	// 4. 加载并启动插件
	ag.loadPlugins(ctx)

	// 5. 消息处理主循环
	go ag.messageLoop(ctx)

	// 6. 监听 mDNS 发现事件
	go ag.discoveryLoop(ctx)

	log.Printf("[agora] started — agent=%s, listening on :%d", ag.self.AgentID, ag.cfg.Port)
	return nil
}

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
			log.Printf("[agora] plugin %s: agent not detected on this machine", name)
			continue
		}
		log.Printf("[agora] plugin %s: detected, connecting...", name)
		if err := p.Connect(ctx); err != nil {
			log.Printf("[agora] plugin %s: connect failed: %v", name, err)
			continue
		}
		// 监听本机 Agent 发出的消息
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

func (ag *Agora) messageLoop(ctx context.Context) {
	for {
		select {
		case msg := <-ag.msgCh:
			// 本机 Agent 发出的消息 → 转发给 Mesh 中的对端
			ag.hub.Broadcast(msg)
		case <-ctx.Done():
			return
		}
	}
}

func (ag *Agora) discoveryLoop(ctx context.Context) {
	for {
		select {
		case entry := <-ag.disco.Entries():
			peerID := entry.Name
			log.Printf("[agora] discovered peer: %s (%s)", peerID, entry.AddrV4)
		case <-ctx.Done():
			return
		}
	}
}

func (ag *Agora) onPeerConn(peerID string) {
	log.Printf("[agora] peer connected: %s", peerID)
}

// Stop 停止 Agora 核心
func (ag *Agora) Stop() {
	ag.cancel()
	ag.disco.Stop()
	ag.hub.Stop()
	for _, p := range ag.plugins {
		p.Close()
	}
	log.Print("[agora] stopped")
}
