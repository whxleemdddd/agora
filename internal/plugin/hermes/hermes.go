package hermes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/whxleem/agora/internal/plugin"
)

func init() {
	plugin.Register("hermes", func() plugin.Plugin {
		return &HermesPlugin{}
	})
}

type HermesPlugin struct {
	configPath string
	apiBase    string
	client     *http.Client
	stopCh     chan struct{}
}

func (h *HermesPlugin) Name() string {
	return "hermes"
}

func (h *HermesPlugin) Detect(ctx context.Context) bool {
	// 策略1: 检查 ~/.hermes/config.yaml 是否存在
	home, _ := os.UserHomeDir()
	candidate := filepath.Join(home, ".hermes", "config.yaml")
	if _, err := os.Stat(candidate); err == nil {
		h.configPath = candidate
		h.apiBase = "http://127.0.0.1:8080"
		return true
	}

	// 策略2: 检查 HERMES_API_URL 环境变量
	if url := os.Getenv("HERMES_API_URL"); url != "" {
		h.configPath = os.Getenv("HERMES_CONFIG")
		h.apiBase = url
		return true
	}

	// 策略3: 检查常见的安装路径
	commonPaths := []string{
		filepath.Join(home, ".local", "bin", "hermes"),
		"/usr/local/bin/hermes",
		"/opt/homebrew/bin/hermes",
	}
	for _, p := range commonPaths {
		if _, err := os.Stat(p); err == nil {
			h.configPath = candidate
			h.apiBase = "http://127.0.0.1:8080"
			return true
		}
	}

	return false
}

func (h *HermesPlugin) Connect(ctx context.Context) error {
	// 检查 Hermes API 是否可达
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(h.apiBase + "/api/health")
	if err != nil {
		// Hermes 没在运行，尝试启动
		home, _ := os.UserHomeDir()
		hermesBin := filepath.Join(home, ".local", "bin", "hermes")
		if _, statErr := os.Stat(hermesBin); statErr != nil {
			return fmt.Errorf("hermes not running and binary not found at %s: %w", hermesBin, err)
		}
		// 用 nohup 在后台启动
		cmd := exec.CommandContext(ctx, "nohup", hermesBin, "start", "&")
		cmd.Stdout = nil
		cmd.Stderr = nil
		if startErr := cmd.Start(); startErr != nil {
			return fmt.Errorf("failed to start hermes: %w", startErr)
		}
		// 等 3 秒让它启动
		time.Sleep(3 * time.Second)
		resp, err = client.Get(h.apiBase + "/api/health")
		if err != nil {
			return fmt.Errorf("hermes started but API not reachable: %w", err)
		}
	}
	defer resp.Body.Close()

	h.client = client
	h.stopCh = make(chan struct{})
	return nil
}

func (h *HermesPlugin) SendToAgent(ctx context.Context, msg []byte) error {
	// 通过 Hermes API /api/chat 注入消息
	req := map[string]interface{}{
		"message":  string(msg),
		"source":   "agora",
		"stream":   false,
	}
	body, _ := json.Marshal(req)
	resp, err := h.client.Post(h.apiBase+"/api/chat", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("send to hermes: %w", err)
	}
	defer resp.Body.Close()
	return nil
}

func (h *HermesPlugin) ListenAgent(ctx context.Context, ch chan<- []byte) (stop func(), err error) {
	// Hermes 目前没有主动推送接口，通过轮询 /api/conversations/last 实现
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		lastID := ""
		for {
			select {
			case <-ticker.C:
				resp, err := h.client.Get(h.apiBase + "/api/conversations/last")
				if err != nil {
					continue
				}
				var result struct {
					ID      string          `json:"id"`
					Content json.RawMessage `json:"content"`
				}
				json.NewDecoder(resp.Body).Decode(&result)
				resp.Body.Close()

				if result.ID != "" && result.ID != lastID {
					lastID = result.ID
					data, _ := json.Marshal(result)
					select {
					case ch <- data:
					default:
					}
				}
			case <-h.stopCh:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	return func() {
		close(h.stopCh)
	}, nil
}

func (h *HermesPlugin) Close() error {
	if h.stopCh != nil {
		close(h.stopCh)
	}
	return nil
}
