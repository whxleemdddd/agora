package hermes

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/whxleem/agora/internal/plugin"
	"github.com/whxleem/agora/pkg/types"
)

func init() {
	plugin.Register("hermes", func() plugin.Plugin {
		return &HermesPlugin{}
	})
}

type HermesPlugin struct {
	stopCh chan struct{}
}

func (h *HermesPlugin) Name() string {
	return "hermes"
}

func (h *HermesPlugin) Detect(ctx context.Context) bool {
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".local", "bin", "hermes"),
		"/usr/local/bin/hermes",
		"/opt/homebrew/bin/hermes",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

func (h *HermesPlugin) Connect(ctx context.Context) error {
	h.stopCh = make(chan struct{})
	log.Printf("[hermes] plugin ready")
	return nil
}

func (h *HermesPlugin) SendToAgent(ctx context.Context, msg []byte) error {
	home, _ := os.UserHomeDir()
	hermesBin := filepath.Join(home, ".local", "bin", "hermes")

	// 解析消息中的文本
	var parsed struct {
		Payload string `json:"payload"`
	}
	_ = json.Unmarshal(msg, &parsed)
	text := parsed.Payload
	if text == "" {
		text = string(msg)
	}

	// 通过 hermes --message 单次问答
	ctx2, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx2, hermesBin, "--message", text)
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("hermes reply: %w", err)
	}

	reply := strings.TrimSpace(string(output))
	if reply == "" {
		return nil
	}

	// 把回复广播出去（通过全局 msgCh 无法直接访问，返回给调用者处理）
	// 改为：写入一个文件或通过回调机制
	// 当前方案：把回复写到 stdout，由调用者（messageLoop）读取
	fmt.Println(reply)
	return nil
}

func (h *HermesPlugin) ListenAgent(ctx context.Context, ch chan<- []byte) (stop func(), err error) {
	// Hermes 目前没有被动触发机制，保持连接活性即可
	return func() {
		close(h.stopCh)
	}, nil
}

func (h *HermesPlugin) Skills() []types.SkillDesc {
	return []types.SkillDesc{
		{Name: "chat", Description: "通用对话能力 — Hermes AI Agent"},
	}
}

func (h *HermesPlugin) Close() error {
	if h.stopCh != nil {
		close(h.stopCh)
	}
	return nil
}
