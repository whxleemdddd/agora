package plugin

import "context"

// Plugin 插件接口 — 每种 Agent 框架实现一个
type Plugin interface {
	// Name 返回插件名称（如 "hermes"）
	Name() string

	// Detect 检测本机是否运行了对应的 Agent
	// 返回 true 表示检测到，Agora 核心会自动执行 Connect
	Detect(ctx context.Context) bool

	// Connect 连接到本机 Agent，建立通信通道
	// 返回 nil 表示连接成功
	Connect(ctx context.Context) error

	// SendToAgent 把 Agora 消息转发给本机 Agent
	SendToAgent(ctx context.Context, msg []byte) error

	// ListenAgent 监听 Agent 发出的消息，通过 ch 传给 Agora 核心
	// 返回的 stop 函数用于停止监听
	ListenAgent(ctx context.Context, ch chan<- []byte) (stop func(), err error)

	// Close 断开与 Agent 的连接
	Close() error
}

// Factory 插件工厂 — 每个插件注册自己的工厂函数
type Factory func() Plugin

var registry = make(map[string]Factory)

// Register 注册插件
func Register(name string, factory Factory) {
	registry[name] = factory
}

// GetRegistered 获取已注册的插件名称列表
func GetRegistered() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	return names
}

// New 按名称创建插件实例
func New(name string) (Plugin, bool) {
	f, ok := registry[name]
	if !ok {
		return nil, false
	}
	return f(), true
}
