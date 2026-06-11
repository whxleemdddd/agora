package message

import (
	"context"
	"log"
)

// Handler 消息处理器
type Handler interface {
	Handle(msg []byte) ([]byte, error)
}

// HandlerFunc 消息处理函数
type HandlerFunc func(msg []byte) ([]byte, error)

func (f HandlerFunc) Handle(msg []byte) ([]byte, error) {
	return f(msg)
}

// Router 消息路由器
type Router struct {
	routes map[string]Handler
}

func NewRouter() *Router {
	return &Router{
		routes: make(map[string]Handler),
	}
}

// Register 注册消息处理器
func (r *Router) Register(msgType string, handler Handler) {
	r.routes[msgType] = handler
}

// Route 路由消息到对应处理器
func (r *Router) Route(msgType string, msg []byte) ([]byte, bool) {
	handler, ok := r.routes[msgType]
	if !ok {
		return nil, false
	}
	resp, err := handler.Handle(msg)
	if err != nil {
		log.Printf("[router] handler error: %v", err)
		return nil, false
	}
	return resp, true
}

// Start 启动路由（当前为空，后续可以加清理逻辑）
func (r *Router) Start(ctx context.Context) {
	<-ctx.Done()
}
