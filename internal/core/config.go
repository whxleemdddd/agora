package core

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/whxleem/agora/pkg/types"
	"gopkg.in/yaml.v3"
)

// DefaultConfigPath 默认配置文件路径
const DefaultConfigPath = "~/.agora/config.yaml"

// AppConfig 完整应用配置
type AppConfig struct {
	Agent  AgentConfig   `yaml:"agent"`
	Agents []AgentEntry  `yaml:"agents,omitempty"` // 本机挂载的多个子Agent
	Mesh   MeshConfig    `yaml:"mesh"`
	Peers  []PeerConfig  `yaml:"peers,omitempty"`
	API    APIConfig     `yaml:"api"`
}

// AgentEntry 本机一个子Agent的声明
type AgentEntry struct {
	Name   string            `yaml:"name"`
	Type   string            `yaml:"type"`           // hermes, claude-code, custom
	Skills []types.SkillDesc `yaml:"skills,omitempty"`
}

// AgentConfig Agent 自身配置
type AgentConfig struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name"`
}

// MeshConfig Mesh 层配置
type MeshConfig struct {
	Port          int  `yaml:"port"`
	HeartbeatSec  int  `yaml:"heartbeat_sec"`
	OfflineSec    int  `yaml:"offline_sec"`
}

// APIConfig API 层配置
type APIConfig struct {
	Port int `yaml:"port"`
	MCP  int `yaml:"mcp_port"`
}

// PeerConfig 持久化的好友/可信对端
type PeerConfig struct {
	AgentID string `yaml:"agent_id"`
	Name    string `yaml:"name"`
	Alias   string `yaml:"alias,omitempty"`
	Trusted bool   `yaml:"trusted"`
}

// DefaultConfig 返回默认配置
func DefaultConfig() AppConfig {
	return AppConfig{
		Agent: AgentConfig{
			ID:   "",
			Name: "",
		},
		Mesh: MeshConfig{
			Port:         7980,
			HeartbeatSec: 30,
			OfflineSec:   90,
		},
		API: APIConfig{
			Port: 7981,
			MCP:  7982,
		},
		Peers: []PeerConfig{},
	}
}

// LoadConfig 加载配置文件
func LoadConfig(path string) (AppConfig, error) {
	expanded := expandPath(path)

	data, err := os.ReadFile(expanded)
	if err != nil {
		if os.IsNotExist(err) {
			// 文件不存在，返回默认配置
			return DefaultConfig(), nil
		}
		return AppConfig{}, fmt.Errorf("read config: %w", err)
	}

	var cfg AppConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return AppConfig{}, fmt.Errorf("parse config: %w", err)
	}

	// 补充默认值
	def := DefaultConfig()
	if cfg.Mesh.Port == 0 {
		cfg.Mesh.Port = def.Mesh.Port
	}
	if cfg.Mesh.HeartbeatSec == 0 {
		cfg.Mesh.HeartbeatSec = def.Mesh.HeartbeatSec
	}
	if cfg.Mesh.OfflineSec == 0 {
		cfg.Mesh.OfflineSec = def.Mesh.OfflineSec
	}
	if cfg.API.Port == 0 {
		cfg.API.Port = def.API.Port
	}
	if cfg.API.MCP == 0 {
		cfg.API.MCP = def.API.MCP
	}

	return cfg, nil
}

// SaveConfig 保存配置
func SaveConfig(cfg AppConfig, path string) error {
	expanded := expandPath(path)

	dir := filepath.Dir(expanded)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := yaml.Marshal(&cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(expanded, data, 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	return nil
}

// AddPeer 添加对端到好友列表
func (cfg *AppConfig) AddPeer(agentID, name string, trusted bool) {
	for i, p := range cfg.Peers {
		if p.AgentID == agentID {
			cfg.Peers[i].Name = name
			cfg.Peers[i].Trusted = trusted
			return
		}
	}
	cfg.Peers = append(cfg.Peers, PeerConfig{
		AgentID: agentID,
		Name:    name,
		Trusted: trusted,
	})
}

// IsTrusted 检查对端是否在可信列表
func (cfg *AppConfig) IsTrusted(agentID string) bool {
	for _, p := range cfg.Peers {
		if p.AgentID == agentID && p.Trusted {
			return true
		}
	}
	return false
}

func expandPath(path string) string {
	if len(path) > 0 && path[0] == '~' {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[1:])
	}
	return path
}
