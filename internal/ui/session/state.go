// Package session 保存桌面端一问一答的状态，并把 core/events 应用到对话上。
//
// 本包不 import mygo。窗口只在 internal/ui 里把这里的状态画出来。
// Apply 只在主线程调用。事件先经过 core/events.Stream，再进入 Apply。
package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/loveRyujin/lantern/core/events"
)

// StatusAwaiting 是确认被暂缓。事件流没有对应的 done 状态，由 FinishResult 记下。
const StatusAwaiting = "awaiting_approval"

// RiskUnfrozen 是旧确认单没有冻结风险说明时界面必须使用的原文。
const RiskUnfrozen = "风险说明：未随确认单冻结"

const (
	// RoleUser 是用户提问。
	RoleUser = "user"
	// RoleAssistant 是模型回复。
	RoleAssistant = "assistant"
)

// Workspace 是侧边栏里的一项。这里只保留标识，不打开数据库。
type Workspace struct {
	ID string
}

// Step 是一条已经显示出来的步骤摘要。
type Step struct {
	ID      string
	Status  string
	Summary string
	Tool    string
}

// Line 是步骤在对话里的一行。状态用中文，工具名和摘要留原样。
func (s Step) Line() string {
	parts := []string{stepStatusLabel(s.Status)}
	if s.Tool != "" {
		parts = append(parts, s.Tool)
	}
	if s.Summary != "" {
		parts = append(parts, s.Summary)
	}
	return strings.Join(parts, " · ")
}

// Message 是对话里的一条。助手消息在 Done 之前用流式 Markdown 画。
type Message struct {
	ID       int
	Role     string
	Text     string
	RunID    string
	Done     bool
	Status   string
	Note     string
	Approval string
	Steps    []Step
}

// Thread 是一个工作区里的当前会话。本张只保留这一条，会话列表是占位。
type Thread struct {
	Messages []Message
}

// State 是主窗口的对话状态。
type State struct {
	Workspaces   []Workspace
	Active       int
	Draft        string
	threads      map[string]*Thread
	StreamingRun string
	Updates      int
	seq          int
}

// New 用工作区列表构造状态。列表不能为空。
func New(workspaces []Workspace) (*State, error) {
	if len(workspaces) == 0 {
		return nil, errors.New("session: 没有工作区")
	}
	for _, ws := range workspaces {
		if strings.TrimSpace(ws.ID) == "" {
			return nil, errors.New("session: 工作区标识为空")
		}
	}
	return &State{
		Workspaces: append([]Workspace(nil), workspaces...),
		threads:    map[string]*Thread{},
	}, nil
}

// ActiveID 返回当前工作区标识。
func (s *State) ActiveID() string {
	if s == nil || s.Active < 0 || s.Active >= len(s.Workspaces) {
		return ""
	}
	return s.Workspaces[s.Active].ID
}

// Switch 切换当前工作区。标识不存在时保持原样并返回 false。
func (s *State) Switch(id string) bool {
	for i, ws := range s.Workspaces {
		if ws.ID == id {
			s.Active = i
			return true
		}
	}
	return false
}

// Thread 返回工作区的会话。没有过消息时也返回空会话。
func (s *State) Thread(id string) *Thread {
	if s.threads == nil {
		s.threads = map[string]*Thread{}
	}
	th := s.threads[id]
	if th == nil {
		th = &Thread{}
		s.threads[id] = th
	}
	return th
}

// Begin 在当前工作区记下提问，并打开一条等待事件的助手消息。
// 已经有运行在流式输出时拒绝新的提问。
func (s *State) Begin(prompt string) (Ask, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return Ask{}, errors.New("session: 问题为空")
	}
	if s.StreamingRun != "" {
		return Ask{}, errors.New("session: 上一问还在输出")
	}
	id := s.ActiveID()
	if id == "" {
		return Ask{}, errors.New("session: 没有工作区")
	}
	runID, err := newRunID()
	if err != nil {
		return Ask{}, err
	}
	th := s.Thread(id)
	s.seq++
	userID := s.seq
	s.seq++
	th.Messages = append(th.Messages,
		Message{ID: userID, Role: RoleUser, Text: prompt, Done: true},
		Message{ID: s.seq, Role: RoleAssistant, RunID: runID},
	)
	s.Draft = ""
	s.StreamingRun = runID
	return Ask{RunID: runID, WorkspaceID: id, Prompt: prompt}, nil
}

// Apply 把一条核心事件写进对应的助手消息。
// 不合法的事件、未知的 run，以及完成后才到的 token，都直接丢掉。
func (s *State) Apply(ev events.Event) {
	if err := ev.Validate(); err != nil {
		return
	}
	msg := s.messageByRun(ev.RunID)
	if msg == nil {
		return
	}
	switch ev.Kind {
	case events.KindToken:
		if msg.Done || ev.Token == nil {
			return
		}
		msg.Text += ev.Token.Text
	case events.KindStep:
		if ev.Step == nil {
			return
		}
		msg.upsert(Step{
			ID:      ev.Step.ID,
			Status:  ev.Step.Status,
			Summary: ev.Step.Summary,
			Tool:    ev.Step.ToolName,
		})
	case events.KindApprovalRequest:
		if ev.Approval == nil {
			return
		}
		msg.Approval = fmt.Sprintf("等待确认 · %s · %s", ev.Approval.Action, ev.Approval.TicketID)
	case events.KindDone:
		msg.Done = true
		if ev.Done != nil {
			msg.Status = ev.Done.Status
			if msg.Text == "" && ev.Done.Text != "" {
				msg.Text = ev.Done.Text
			}
		}
		if s.StreamingRun == ev.RunID {
			s.StreamingRun = ""
		}
	default:
		return
	}
	s.Updates++
}

// Finish 在运行返回后收尾。事件里已经有 Done 时不再改状态。
// 事件流没有把这条运行收完时，记成失败，避免界面一直停在流式中。
func (s *State) Finish(runID string, err error) {
	s.FinishResult(runID, err, "")
}

// NoteRun 用纯文本注明恢复结果，不写入模型回答。
func (s *State) NoteRun(runID, status, note string) {
	msg := s.messageByRun(runID)
	if msg == nil {
		return
	}
	msg.Done = true
	if status != "" {
		msg.Status = status
	}
	if note != "" {
		msg.Note = note
	}
	if s.StreamingRun == runID {
		s.StreamingRun = ""
	}
	s.Updates++
}

// FinishResult 与 Finish 相同。runState 为 awaiting_approval 时记成等待确认，
// 不把暂缓画成失败。
func (s *State) FinishResult(runID string, err error, runState string) {
	msg := s.messageByRun(runID)
	if msg == nil {
		if s.StreamingRun == runID {
			s.StreamingRun = ""
		}
		return
	}
	if runState == StatusAwaiting && !msg.Done {
		msg.Done = true
		msg.Status = StatusAwaiting
		if s.StreamingRun == runID {
			s.StreamingRun = ""
		}
		return
	}
	if err != nil {
		msg.Note = err.Error()
	}
	if !msg.Done {
		msg.Done = true
		if msg.Status == "" {
			msg.Status = events.DoneFailed
		}
	}
	if s.StreamingRun == runID {
		s.StreamingRun = ""
	}
}

// Phase 是顶栏的短状态。
func (s *State) Phase() string {
	if s.StreamingRun != "" {
		return "流式中"
	}
	msg := s.latestAssistant(s.ActiveID())
	if msg == nil || !msg.Done {
		return "等待提问"
	}
	switch msg.Status {
	case events.DoneCompleted:
		return "已完成"
	case events.DoneFailed:
		return "失败"
	case events.DoneCancelled:
		return "已取消"
	case events.DoneRejected:
		return "已拒绝"
	case events.DoneExpired:
		return "已超时"
	case StatusAwaiting:
		return "等待确认"
	default:
		if msg.Status == "" {
			return "已完成"
		}
		return msg.Status
	}
}

func (s *State) latestAssistant(workspaceID string) *Message {
	th := s.threads[workspaceID]
	if th == nil {
		return nil
	}
	for i := len(th.Messages) - 1; i >= 0; i-- {
		if th.Messages[i].Role == RoleAssistant {
			return &th.Messages[i]
		}
	}
	return nil
}

func (s *State) messageByRun(runID string) *Message {
	if runID == "" {
		return nil
	}
	for _, th := range s.threads {
		for i := range th.Messages {
			if th.Messages[i].RunID == runID {
				return &th.Messages[i]
			}
		}
	}
	return nil
}

func (m *Message) upsert(step Step) {
	for i := range m.Steps {
		if m.Steps[i].ID == step.ID {
			m.Steps[i] = step
			return
		}
	}
	m.Steps = append(m.Steps, step)
}

func newRunID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("session: 生成运行标识: %w", err)
	}
	return "ui-" + hex.EncodeToString(buf[:]), nil
}

// RiskLine 返回要展示的风险原文。空风险不用别的文案顶替。
func RiskLine(risk string) string {
	if strings.TrimSpace(risk) == "" {
		return RiskUnfrozen
	}
	return "风险 " + risk
}

func stepStatusLabel(status string) string {
	switch status {
	case events.StepExecuting:
		return "进行中"
	case events.StepDone:
		return "完成"
	case events.StepError:
		return "出错"
	default:
		return status
	}
}
