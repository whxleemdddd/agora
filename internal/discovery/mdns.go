package discovery

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/hashicorp/mdns"
)

const ServiceName = "_agora._tcp"
const ServicePort = 7980

// PeerFound 对端发现事件
type PeerFound struct {
	IP       string `json:"ip"`
	Port     int    `json:"port"`
	AgentID  string `json:"agent_id"`
	Name     string `json:"name"`
}

// Discoverer mDNS 发现服务
type Discoverer struct {
	server  *mdns.Server
	entries chan *mdns.ServiceEntry
}

// NewDiscoverer 创建发现器
func NewDiscoverer() *Discoverer {
	return &Discoverer{
		entries: make(chan *mdns.ServiceEntry, 32),
	}
}

// Start 启动 mDNS 服务端（广播自身）和客户端（监听对端）
func (d *Discoverer) Start(ctx context.Context, agentID, name string) error {
	// 获取本机局域网 IP
	ip, err := getLocalIP()
	if err != nil {
		return fmt.Errorf("get local ip: %w", err)
	}

	// 构建 mDNS service
	service, err := mdns.NewMDNSService(
		agentID,
		ServiceName,
		"",        // domain
		"",        // host
		ServicePort,
		[]net.IP{ip},
		[]string{fmt.Sprintf("name=%s", name)},
	)
	if err != nil {
		return fmt.Errorf("new mdns service: %w", err)
	}

	// 启动 mDNS server（广播自身）
	server, err := mdns.NewServer(&mdns.Config{Zone: service})
	if err != nil {
		return fmt.Errorf("start mdns server: %w", err)
	}
	d.server = server

	// 启动 mDNS client（监听对端）
	go d.startQueryLoop(ctx)

	return nil
}

func (d *Discoverer) startQueryLoop(ctx context.Context) {
	// 每 30 秒查询一次局域网内的 Agora 服务
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// 立即执行一次
	d.queryOnce()

	for {
		select {
		case <-ticker.C:
			d.queryOnce()
		case <-ctx.Done():
			return
		}
	}
}

func (d *Discoverer) queryOnce() {
	entriesCh := make(chan *mdns.ServiceEntry, 16)
	go func() {
		for entry := range entriesCh {
			d.entries <- entry
		}
	}()

	mdns.Lookup(ServiceName, entriesCh)
	close(entriesCh)
}

// Entries 返回发现事件 channel
func (d *Discoverer) Entries() <-chan *mdns.ServiceEntry {
	return d.entries
}

// Stop 停止 mDNS 服务
func (d *Discoverer) Stop() error {
	if d.server != nil {
		return d.server.Shutdown()
	}
	return nil
}

// getLocalIP 获取本机局域网 IP
func getLocalIP() (net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP, nil
			}
		}
	}
	return nil, fmt.Errorf("no local IP found")
}
