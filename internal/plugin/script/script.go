package script

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/whxleem/agora/internal/plugin"
)

func init() {
	plugin.Register("custom-script", func() plugin.Plugin {
		return &ScriptPlugin{}
	})
}

type ScriptPlugin struct {
	proc    *exec.Cmd
	writer  *bufio.Writer
	stopCh  chan struct{}
	script  string
}

func (s *ScriptPlugin) Name() string {
	return "custom-script"
}

func (s *ScriptPlugin) Detect(ctx context.Context) bool {
	// 检测 ~/.agora/scripts/ 下是否有 .agora.json 定义文件
	home, _ := os.UserHomeDir()
	scriptsDir := filepath.Join(home, ".agora", "scripts")
	entries, err := os.ReadDir(scriptsDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".agora.json") {
			s.script = filepath.Join(scriptsDir, e.Name())
			return true
		}
	}
	return false
}

func (s *ScriptPlugin) Connect(ctx context.Context) error {
	if s.script == "" {
		return fmt.Errorf("no custom script defined")
	}

	// 读取脚本定义
	defData, err := os.ReadFile(s.script)
	if err != nil {
		return fmt.Errorf("read script def: %w", err)
	}
	var def struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
		WorkDir string   `json:"workdir,omitempty"`
	}
	if err := json.Unmarshal(defData, &def); err != nil {
		return fmt.Errorf("parse script def: %w", err)
	}

	s.proc = exec.CommandContext(ctx, def.Command, def.Args...)
	if def.WorkDir != "" {
		s.proc.Dir = def.WorkDir
	}

	stdin, err := s.proc.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	s.writer = bufio.NewWriter(stdin)
	s.proc.Stdout = os.Stdout
	s.proc.Stderr = os.Stderr

	if err := s.proc.Start(); err != nil {
		return fmt.Errorf("start script: %w", err)
	}

	s.stopCh = make(chan struct{})
	return nil
}

func (s *ScriptPlugin) SendToAgent(ctx context.Context, msg []byte) error {
	if s.writer == nil {
		return fmt.Errorf("not connected")
	}
	_, err := s.writer.Write(append(msg, '\n'))
	if err != nil {
		return fmt.Errorf("write to script stdin: %w", err)
	}
	return s.writer.Flush()
}

func (s *ScriptPlugin) ListenAgent(ctx context.Context, ch chan<- []byte) (stop func(), err error) {
	go func() {
		// 脚本通过 stdout 输出 JSON 行即可
		scanner := bufio.NewScanner(s.proc.Stdout.(*os.File))
		for scanner.Scan() {
			line := scanner.Bytes()
			msg := make([]byte, len(line))
			copy(msg, line)
			select {
			case ch <- msg:
			default:
			}
		}
	}()

	return func() {
		close(s.stopCh)
	}, nil
}

func (s *ScriptPlugin) Close() error {
	if s.proc != nil && s.proc.Process != nil {
		return s.proc.Process.Kill()
	}
	return nil
}
