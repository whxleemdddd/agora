package core

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/whxleem/agora/pkg/types"
)

// ── SkillRegistry ────────────────────────────────────

// SkillRegistry 全局技能注册表
type SkillRegistry struct {
	mu      sync.RWMutex
	local   map[string]types.SkillDesc
	remote  map[string]map[string]bool
}

func NewSkillRegistry() *SkillRegistry {
	return &SkillRegistry{
		local:  make(map[string]types.SkillDesc),
		remote: make(map[string]map[string]bool),
	}
}

func (sr *SkillRegistry) RegisterLocal(skill types.SkillDesc) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.local[skill.Name] = skill
	log.Printf("[skill] registered local: %s — %s", skill.Name, skill.Description)
}

func (sr *SkillRegistry) RegisterRemote(agentID string, skills []types.SkillDesc) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	m := make(map[string]bool)
	for _, s := range skills {
		m[s.Name] = true
	}
	sr.remote[agentID] = m
	log.Printf("[skill] registered remote %s: %d skills", agentID, len(skills))
}

func (sr *SkillRegistry) UnregisterRemote(agentID string) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	delete(sr.remote, agentID)
}

func (sr *SkillRegistry) FindLocal(skillName string) bool {
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	_, ok := sr.local[skillName]
	return ok
}

func (sr *SkillRegistry) FindRemote(skillName string) []string {
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	var agents []string
	for agentID, skills := range sr.remote {
		if skills[skillName] {
			agents = append(agents, agentID)
		}
	}
	return agents
}

// ── TaskManager ──────────────────────────────────────

type TaskManager struct {
	mu    sync.RWMutex
	tasks map[string]*TaskRecord
}

type TaskRecord struct {
	ID        string
	From      string
	To        string
	Skill     string
	Payload   json.RawMessage
	Status    string // pending / running / success / failed
	Result    json.RawMessage
	CreatedAt time.Time
}

func NewTaskManager() *TaskManager {
	return &TaskManager{
		tasks: make(map[string]*TaskRecord),
	}
}

func (tm *TaskManager) NewTask(from, to, skill string, payload json.RawMessage) *TaskRecord {
	t := &TaskRecord{
		ID:        uuid.New().String()[:12],
		From:      from,
		To:        to,
		Skill:     skill,
		Payload:   payload,
		Status:    "pending",
		CreatedAt: time.Now(),
	}
	tm.mu.Lock()
	tm.tasks[t.ID] = t
	tm.mu.Unlock()
	return t
}

func (tm *TaskManager) Complete(id string, result json.RawMessage) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if t, ok := tm.tasks[id]; ok {
		t.Status = "success"
		t.Result = result
	}
}

func (tm *TaskManager) Fail(id string, errMsg string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if t, ok := tm.tasks[id]; ok {
		t.Status = "failed"
		t.Result, _ = json.Marshal(map[string]string{"error": errMsg})
	}
}

// ── 消息处理（task 入口）────────────────────────────

func (ag *Agora) handleTask(ctx context.Context, msg types.Message) {
	var task struct {
		Skill   string          `json:"skill"`
		Payload json.RawMessage `json:"payload"`
	}
	payloadData, _ := json.Marshal(msg.Payload)
	json.Unmarshal(payloadData, &task)

	log.Printf("[task] received: skill=%s from=%s id=%s", task.Skill, msg.From, msg.ID)

	if !ag.skills.FindLocal(task.Skill) {
		errResp := types.Message{
			Type:      types.MsgError,
			From:      ag.self.AgentID,
			To:        msg.From,
			ID:        "resp-" + msg.ID,
			Timestamp: time.Now(),
			Payload:   map[string]string{"error": "skill not found: " + task.Skill},
		}
		errData, _ := json.Marshal(errResp)
		ag.hub.SendTo(msg.From, errData)
		return
	}

	record := ag.tasks.NewTask(msg.From, ag.self.AgentID, task.Skill, task.Payload)
	log.Printf("[task] task %s created, skill=%s", record.ID, task.Skill)
}
