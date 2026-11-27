package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo"
	mygoui "github.com/egoist/mygo/ui"
	"github.com/loveRyujin/lantern/core/approval"
)

// quitGrace 是「仍退出」后等后台 Loop 和 Resume 落库的上限。
// 只等 ctx 取消后的收尾写入，不等工具跑完；和沙箱墙钟上限是两回事。超时由下次启动扫描兜底。
const quitGrace = 5 * time.Second

type quitPhase int

const (
	quitIdle quitPhase = iota
	quitAsking
	quitLeaving
	// quitExiting：有界等待已结束，只剩 leave 自己发起的那次 Quit。之前的阶段一律拦住，不跳过等待。
	quitExiting
)

// quitWork 是本进程内会被退出打断的工作。不按库扫描：崩溃残留的 running 行不代表这里有人在跑。
type quitWork struct {
	waiting   []approval.Ticket
	streaming int
	resuming  int
}

func (w quitWork) empty() bool {
	return len(w.waiting) == 0 && w.streaming == 0 && w.resuming == 0
}

// activity 合并等待器里的确认单与 Resume，以及 shell 自己记下的提问和重开。
// 一趟运行若正在等决定，只算作等待中的确认单。
func (s *shell) activity() quitWork {
	a := s.waiter.Activity()
	waitingRuns := map[string]bool{}
	for _, tk := range a.Waiting {
		waitingRuns[tk.RunID] = true
	}
	resuming := map[string]bool{}
	for _, id := range a.Resuming {
		resuming[id] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.resumes {
		resuming[id] = true
	}
	w := quitWork{waiting: a.Waiting}
	for id := range s.asks {
		if !waitingRuns[id] {
			w.streaming++
		}
	}
	for id := range resuming {
		if !waitingRuns[id] {
			w.resuming++
		}
	}
	return w
}

// allowQuit 由 Window.OnClose 和 App.OnBeforeQuit 共用，都在主线程同步调用。
// 返回 false 时调用方 PreventDefault。同一次退出两个回调先后到来，拦截框只开一个。
func (s *shell) allowQuit() bool {
	s.mu.Lock()
	phase := s.quit
	s.mu.Unlock()
	switch phase {
	case quitExiting:
		return true
	case quitAsking, quitLeaving:
		return false
	}
	w := s.activity()
	if w.empty() {
		return true
	}
	s.mu.Lock()
	if s.quit == quitIdle {
		s.quit, s.quitWork = quitAsking, w
	}
	s.mu.Unlock()
	if s.win != nil {
		s.win.Invalidate()
	}
	return false
}

func (s *shell) onBeforeQuit(e *mygo.QuitEvent) {
	if !s.allowQuit() {
		e.PreventDefault()
	}
}

func (s *shell) onClose(e *mygo.CloseEvent) {
	if !s.allowQuit() {
		e.PreventDefault()
	}
}

func (s *shell) quitAsked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quit == quitAsking
}

func (s *shell) leaving() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quit >= quitLeaving
}

// quitDialog 画拦截框。它打开时不画下面的确认框，Enter 和 Escape 只落在这里。
// 最后一个按钮是 Enter 默认项，所以「取消退出」放最后。
func (s *shell) quitDialog(c *mygoui.Context) {
	s.mu.Lock()
	if s.quit != quitAsking {
		s.mu.Unlock()
		return
	}
	w := s.quitWork
	active := s.state.ActiveID()
	s.mu.Unlock()
	open := true
	choice := mygoui.AlertDialog(c, &open, "仍要退出 lantern？", quitMessage(w, active), "仍退出", "取消退出")
	switch choice {
	case 0:
		s.leave()
	case 1, mygoui.AlertDismissed:
		s.mu.Lock()
		s.quit = quitIdle
		s.mu.Unlock()
	}
}

func quitMessage(w quitWork, active string) string {
	var lines []string
	if n := len(w.waiting); n > 0 {
		elsewhere := 0
		for _, tk := range w.waiting {
			if tk.WorkspaceID != active {
				elsewhere++
			}
		}
		line := fmt.Sprintf("%d 张确认单在等待决定", n)
		if elsewhere > 0 {
			line += fmt.Sprintf("（其中 %d 张不在当前工作区）", elsewhere)
		}
		lines = append(lines, line+"：退出时暂缓保留，下次启动仍在待确认列表里。")
	}
	if w.streaming > 0 {
		lines = append(lines, fmt.Sprintf("%d 个回答正在进行：退出时取消，不会续跑。", w.streaming))
	}
	if w.resuming > 0 {
		lines = append(lines, fmt.Sprintf("%d 个确认单正在恢复执行：执行中的工具会被中止，不会重跑。", w.resuming))
	}
	lines = append(lines,
		"等待中的确认单会暂缓保留，进行中的回答会被取消，执行中的工具会被中止且不会重跑。",
		fmt.Sprintf("退出前最多等 %d 秒让这些运行落库。", int(quitGrace/time.Second)))
	return strings.Join(lines, "\n")
}

// leave 执行「仍退出」：先给所有等待中的确认单送暂缓，再取消其余运行的 ctx，
// 有界等待后台收尾，然后退出。超时也退出，留给下次启动扫描。
func (s *shell) leave() {
	s.mu.Lock()
	if s.quit >= quitLeaving {
		s.mu.Unlock()
		return
	}
	s.quit = quitLeaving
	s.mu.Unlock()
	deferred := s.waiter.Shutdown()
	s.stop()
	grace := s.graceLimit()
	s.mu.Lock()
	s.asking = false
	s.notice = fmt.Sprintf("正在退出：已暂缓 %d 张确认单，最多等 %d 秒让运行落库。", len(deferred), int(grace/time.Second))
	s.mu.Unlock()
	go func() {
		s.waitWork(grace)
		s.mu.Lock()
		s.quit = quitExiting
		s.mu.Unlock()
		if s.exit != nil {
			s.exit()
		}
	}()
}

func (s *shell) graceLimit() time.Duration {
	if s.grace > 0 {
		return s.grace
	}
	return quitGrace
}

// finishCheck 收尾 -check：先等自动提问落完（留下的已暂缓票不会被改写），再按「仍退出」退出。
// 自动提问失败时可能还有回答在流式或确认单在等，直接 Quit 会被拦截框拦住，无人值守的进程就挂着。
func (s *shell) finishCheck(err error) error {
	s.waitWork(s.graceLimit())
	s.leave()
	return err
}

// beginWork 记一项后台工作。退出已经开始时拒绝，保证 Add 先于 waitWork。
func (s *shell) beginWork() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.quit >= quitLeaving {
		return false
	}
	s.work.Add(1)
	return true
}

// waitWork 最多等 d，返回后台工作是否都已结束。
func (s *shell) waitWork(d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.work.Wait()
		close(done)
	}()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
