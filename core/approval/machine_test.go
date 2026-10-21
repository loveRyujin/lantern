package approval

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/errcode"
	_ "modernc.org/sqlite"
)

func TestLegalTransitions(t *testing.T) {
	ctx := t.Context()

	t.Run("pending 到 approved", func(t *testing.T) {
		db := openApprovalDB(t)
		m := New(db)
		ticket := mustCreatePending(t, ctx, m, db, "ok-approve", []byte(`{"object":"contacts"}`), time.Now().Add(time.Hour))
		before := auditCount(t, db)
		if err := m.Decide(ctx, ticket.ID, Decision{Action: ActionApprove}); err != nil {
			t.Fatal(err)
		}
		got := readApproval(t, db, ticket.ID)
		if got.state != string(StateApproved) || got.version != 1 || got.decision != string(ActionApprove) || !got.decided.Valid || got.consumed.Valid {
			t.Fatalf("批准后 %+v", got)
		}
		if runState(t, db, ticket.RunID) != "running" {
			t.Fatalf("合法批准不该把运行标成失败: %s", runState(t, db, ticket.RunID))
		}
		if auditCount(t, db) != before {
			t.Fatal("合法转移不写审计")
		}
	})

	t.Run("pending 到 rejected", func(t *testing.T) {
		db := openApprovalDB(t)
		m := New(db)
		ticket := mustCreatePending(t, ctx, m, db, "ok-reject", []byte(`{"object":"contacts"}`), time.Now().Add(time.Hour))
		if err := m.Decide(ctx, ticket.ID, Decision{Action: ActionReject}); err != nil {
			t.Fatal(err)
		}
		got := readApproval(t, db, ticket.ID)
		if got.state != string(StateRejected) || got.version != 1 || got.decision != string(ActionReject) || !got.decided.Valid || got.consumed.Valid {
			t.Fatalf("拒绝后 %+v", got)
		}
	})

	t.Run("pending 到 expired", func(t *testing.T) {
		db := openApprovalDB(t)
		m := New(db)
		exp := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
		m.SetClock(func() time.Time { return exp.Add(-time.Minute) })
		ticket := mustCreatePending(t, ctx, m, db, "ok-expire", []byte(`{"object":"contacts"}`), exp)
		m.SetClock(func() time.Time { return exp })
		if err := m.Expire(ctx, ticket.ID); err != nil {
			t.Fatal(err)
		}
		got := readApproval(t, db, ticket.ID)
		if got.state != string(StateExpired) || got.version != 1 || got.decision != "" || got.decided.Valid || got.consumed.Valid {
			t.Fatalf("过期后 %+v", got)
		}
	})

	t.Run("approved 到 consumed", func(t *testing.T) {
		db := openApprovalDB(t)
		m := New(db)
		ticket := mustApprove(t, ctx, m, db, "ok-consume", []byte(`{"object":"contacts"}`))
		if err := m.Consume(ctx, ticket.ID, contextOf(ticket)); err != nil {
			t.Fatal(err)
		}
		got := readApproval(t, db, ticket.ID)
		if got.state != string(StateConsumed) || got.version != 2 || got.decision != string(ActionApprove) || !got.consumed.Valid {
			t.Fatalf("消费后 %+v", got)
		}
		if runState(t, db, ticket.RunID) != "running" {
			t.Fatalf("成功消费不该把运行标成失败: %s", runState(t, db, ticket.RunID))
		}
	})

	t.Run("approved 到 expired", func(t *testing.T) {
		db := openApprovalDB(t)
		m := New(db)
		exp := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
		m.SetClock(func() time.Time { return exp.Add(-time.Minute) })
		ticket := mustApprove(t, ctx, m, db, "ok-approved-expire", []byte(`{"object":"contacts"}`))
		if _, err := db.ExecContext(ctx, `UPDATE approvals SET expires_at = ? WHERE id = ?`, exp.Format(time.RFC3339Nano), ticket.ID); err != nil {
			t.Fatal(err)
		}
		m.SetClock(func() time.Time { return exp })
		if err := m.Expire(ctx, ticket.ID); err != nil {
			t.Fatal(err)
		}
		got := readApproval(t, db, ticket.ID)
		if got.state != string(StateExpired) || got.version != 2 || got.decision != string(ActionApprove) || got.consumed.Valid {
			t.Fatalf("已批准过期后 %+v", got)
		}
	})
}

func TestIllegalTransitionsLeaveStateAndWriteOneAudit(t *testing.T) {
	ctx := t.Context()
	db := openApprovalDB(t)
	m := New(db)
	states := []State{StatePending, StateApproved, StateRejected, StateExpired, StateConsumed}
	for _, from := range states {
		for _, to := range states {
			if legal(from, to) {
				continue
			}
			from, to := from, to
			t.Run(string(from)+"到"+string(to), func(t *testing.T) {
				id := string(from) + "-" + string(to)
				ticket := mustCreatePending(t, ctx, m, db, id, []byte(`{"phone":"13800138000"}`), time.Now().Add(time.Hour))
				if _, err := db.ExecContext(ctx, `UPDATE approvals SET state = ? WHERE id = ?`, string(from), ticket.ID); err != nil {
					t.Fatal(err)
				}
				rec, err := load(ctx, db, ticket.ID)
				if err != nil {
					t.Fatal(err)
				}
				before := auditsOf(t, db)
				err = m.transition(ctx, db, rec, to, rec.decision, rec.decidedAt, rec.consumedAt)
				if !errors.Is(err, ErrIllegalTransition) {
					t.Fatalf("错误 = %v", err)
				}
				if terminal(from) && !errors.Is(err, ErrTerminal) {
					t.Fatalf("终态上的非法转移应能被 ErrTerminal 认出来: %v", err)
				}
				if !terminal(from) && errors.Is(err, ErrTerminal) {
					t.Fatalf("非终态不应被 ErrTerminal 认出来: %v", err)
				}
				if errors.Is(err, ErrVersionConflict) || errors.Is(err, ErrMismatch) {
					t.Fatalf("非法转移不应报成版本冲突或核对不一致: %v", err)
				}
				got := readApproval(t, db, ticket.ID)
				if got.state != string(from) || got.version != rec.version {
					t.Fatalf("状态被改了: %+v", got)
				}
				added := auditsOf(t, db)[len(before):]
				if len(added) != 1 || added[0].action != "illegal_transition" {
					t.Fatalf("审计 = %+v", added)
				}
				if added[0].detail == "" || containsPhone(added[0].detail) || containsPhone(err.Error()) {
					t.Fatalf("审计或错误带了参数: %q %v", added[0].detail, err)
				}
			})
		}
	}
}

func TestPublicIllegalPaths(t *testing.T) {
	ctx := t.Context()
	params := []byte(`{"phone":"13800138000"}`)

	t.Run("再批准一次", func(t *testing.T) {
		db := openApprovalDB(t)
		m := New(db)
		ticket := mustApprove(t, ctx, m, db, "twice", params)
		first := readApproval(t, db, ticket.ID)
		before := auditCount(t, db)
		err := m.Decide(ctx, ticket.ID, Decision{Action: ActionApprove})
		if !errors.Is(err, ErrIllegalTransition) || errors.Is(err, ErrTerminal) {
			t.Fatalf("再次批准 = %v", err)
		}
		got := readApproval(t, db, ticket.ID)
		if got.state != string(StateApproved) || got.version != first.version || got.decided != first.decided {
			t.Fatalf("再次批准改了决定时间或版本: before=%+v after=%+v", first, got)
		}
		if auditCount(t, db) != before+1 {
			t.Fatalf("审计条数 = %d", auditCount(t, db))
		}
	})

	t.Run("拒绝后再批准", func(t *testing.T) {
		assertPublicIllegal(t, ctx, params, "rej-app", func(ctx context.Context, m *Machine, db *sql.DB, ticket Ticket) {
			if err := m.Decide(ctx, ticket.ID, Decision{Action: ActionReject}); err != nil {
				t.Fatal(err)
			}
		}, func(ctx context.Context, m *Machine, ticket Ticket) error {
			return m.Decide(ctx, ticket.ID, Decision{Action: ActionApprove})
		}, StateRejected, true)
	})

	t.Run("消费后再批准", func(t *testing.T) {
		assertPublicIllegal(t, ctx, params, "con-app", func(ctx context.Context, m *Machine, db *sql.DB, ticket Ticket) {
			if err := m.Decide(ctx, ticket.ID, Decision{Action: ActionApprove}); err != nil {
				t.Fatal(err)
			}
			if err := m.Consume(ctx, ticket.ID, contextOf(ticket)); err != nil {
				t.Fatal(err)
			}
		}, func(ctx context.Context, m *Machine, ticket Ticket) error {
			return m.Decide(ctx, ticket.ID, Decision{Action: ActionApprove})
		}, StateConsumed, true)
	})

	t.Run("过期后再批准", func(t *testing.T) {
		assertPublicIllegal(t, ctx, params, "exp-app", func(ctx context.Context, m *Machine, db *sql.DB, ticket Ticket) {
			exp := time.Now().Add(-time.Minute).UTC()
			if _, err := db.ExecContext(ctx, `UPDATE approvals SET expires_at = ? WHERE id = ?`, exp.Format(time.RFC3339Nano), ticket.ID); err != nil {
				t.Fatal(err)
			}
			if err := m.Expire(ctx, ticket.ID); err != nil {
				t.Fatal(err)
			}
		}, func(ctx context.Context, m *Machine, ticket Ticket) error {
			return m.Decide(ctx, ticket.ID, Decision{Action: ActionApprove})
		}, StateExpired, true)
	})

	t.Run("pending 不能直接消费", func(t *testing.T) {
		assertPublicIllegal(t, ctx, params, "pend-con", func(context.Context, *Machine, *sql.DB, Ticket) {}, func(ctx context.Context, m *Machine, ticket Ticket) error {
			return m.Consume(ctx, ticket.ID, contextOf(ticket))
		}, StatePending, false)
	})

	t.Run("已批准不能改成拒绝", func(t *testing.T) {
		assertPublicIllegal(t, ctx, params, "app-rej", func(ctx context.Context, m *Machine, db *sql.DB, ticket Ticket) {
			if err := m.Decide(ctx, ticket.ID, Decision{Action: ActionApprove}); err != nil {
				t.Fatal(err)
			}
		}, func(ctx context.Context, m *Machine, ticket Ticket) error {
			return m.Decide(ctx, ticket.ID, Decision{Action: ActionReject})
		}, StateApproved, false)
	})

	t.Run("已消费不能过期", func(t *testing.T) {
		assertPublicIllegal(t, ctx, params, "con-exp", func(ctx context.Context, m *Machine, db *sql.DB, ticket Ticket) {
			if err := m.Decide(ctx, ticket.ID, Decision{Action: ActionApprove}); err != nil {
				t.Fatal(err)
			}
			if err := m.Consume(ctx, ticket.ID, contextOf(ticket)); err != nil {
				t.Fatal(err)
			}
		}, func(ctx context.Context, m *Machine, ticket Ticket) error {
			return m.Expire(ctx, ticket.ID)
		}, StateConsumed, true)
	})

	t.Run("已消费不能再消费", func(t *testing.T) {
		assertPublicIllegal(t, ctx, params, "con-con", func(ctx context.Context, m *Machine, db *sql.DB, ticket Ticket) {
			if err := m.Decide(ctx, ticket.ID, Decision{Action: ActionApprove}); err != nil {
				t.Fatal(err)
			}
			if err := m.Consume(ctx, ticket.ID, contextOf(ticket)); err != nil {
				t.Fatal(err)
			}
		}, func(ctx context.Context, m *Machine, ticket Ticket) error {
			return m.Consume(ctx, ticket.ID, contextOf(ticket))
		}, StateConsumed, true)
	})
}

func TestStaleVersionDoesNotWrite(t *testing.T) {
	ctx := t.Context()
	db := openApprovalDB(t)
	m := New(db)
	ticket := mustCreatePending(t, ctx, m, db, "stale", []byte(`{"object":"contacts"}`), time.Now().Add(time.Hour))
	rec, err := load(ctx, db, ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE approvals SET version = version + 1 WHERE id = ?`, ticket.ID); err != nil {
		t.Fatal(err)
	}
	before := readApproval(t, db, ticket.ID)
	audits := auditCount(t, db)
	decided := sql.NullString{String: time.Now().UTC().Format(time.RFC3339Nano), Valid: true}
	err = m.transition(ctx, db, rec, StateApproved, string(ActionApprove), decided, sql.NullString{})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("过期版本 = %v", err)
	}
	if errors.Is(err, ErrIllegalTransition) {
		t.Fatal("版本冲突不应记成非法转移")
	}
	got := readApproval(t, db, ticket.ID)
	if got != before {
		t.Fatalf("版本冲突改了行: before=%+v after=%+v", before, got)
	}
	if auditCount(t, db) != audits {
		t.Fatal("版本冲突不应写审计")
	}
}

func TestConcurrentApproveAndReject(t *testing.T) {
	ctx := t.Context()
	db := openApprovalDB(t)
	m := New(db)
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("ar-%d", i)
		ticket := mustCreatePending(t, ctx, m, db, id, []byte(`{"object":"contacts"}`), time.Now().Add(time.Hour))
		g := newPairGate()
		m.SetGate(g.enter)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs[0] = m.Decide(ctx, ticket.ID, Decision{Action: ActionApprove})
		}()
		go func() {
			defer wg.Done()
			errs[1] = m.Decide(ctx, ticket.ID, Decision{Action: ActionReject})
		}()
		wg.Wait()
		m.SetGate(nil)
		if g.arrived() != 2 {
			t.Fatalf("第 %d 轮没有停在同一版本", i)
		}
		got := readApproval(t, db, ticket.ID)
		switch {
		case errs[0] == nil && errors.Is(errs[1], ErrVersionConflict):
			if got.state != string(StateApproved) || got.decision != string(ActionApprove) {
				t.Fatalf("第 %d 轮批准胜出但状态是 %+v", i, got)
			}
		case errs[1] == nil && errors.Is(errs[0], ErrVersionConflict):
			if got.state != string(StateRejected) || got.decision != string(ActionReject) {
				t.Fatalf("第 %d 轮拒绝胜出但状态是 %+v", i, got)
			}
		default:
			t.Fatalf("第 %d 轮错误 = %v / %v", i, errs[0], errs[1])
		}
		if got.version != 1 {
			t.Fatalf("第 %d 轮版本 = %d", i, got.version)
		}
		if errors.Is(errs[0], ErrIllegalTransition) || errors.Is(errs[1], ErrIllegalTransition) || auditCount(t, db) != 0 {
			t.Fatalf("第 %d 轮版本冲突被记成非法转移", i)
		}
	}
}

func TestConcurrentConsumeOneWins(t *testing.T) {
	ctx := t.Context()
	db := openApprovalDB(t)
	m := New(db)
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("cc-%d", i)
		ticket := mustApprove(t, ctx, m, db, id, []byte(`{"object":"contacts"}`))
		cc := contextOf(ticket)
		g := newPairGate()
		m.SetGate(g.enter)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs[0] = m.Consume(ctx, ticket.ID, cc)
		}()
		go func() {
			defer wg.Done()
			errs[1] = m.Consume(ctx, ticket.ID, cc)
		}()
		wg.Wait()
		m.SetGate(nil)
		if g.arrived() != 2 {
			t.Fatalf("第 %d 轮没有同时消费", i)
		}
		wins := 0
		for _, err := range errs {
			if err == nil {
				wins++
				continue
			}
			if errors.Is(err, ErrMismatch) || !errors.Is(err, ErrVersionConflict) {
				t.Fatalf("第 %d 轮失败方 = %v", i, err)
			}
		}
		if wins != 1 {
			t.Fatalf("第 %d 轮成功 %d 次", i, wins)
		}
		got := readApproval(t, db, ticket.ID)
		if got.state != string(StateConsumed) || got.version != 2 {
			t.Fatalf("第 %d 轮 %+v", i, got)
		}
	}
}

func TestMismatchDoesNotConsumeOrConflict(t *testing.T) {
	ctx := t.Context()
	db := openApprovalDB(t)
	m := New(db)
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("mm-%d", i)
		ticket := mustApprove(t, ctx, m, db, id, []byte(`{"object":"contacts"}`))
		good := contextOf(ticket)
		bad := good
		bad.RunID = "other-run"
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs[0] = m.Consume(ctx, ticket.ID, good)
		}()
		go func() {
			defer wg.Done()
			errs[1] = m.Consume(ctx, ticket.ID, bad)
		}()
		wg.Wait()
		if errs[0] != nil || !errors.Is(errs[1], ErrMismatch) || errors.Is(errs[1], ErrVersionConflict) {
			t.Fatalf("第 %d 轮 good=%v bad=%v", i, errs[0], errs[1])
		}
		got := readApproval(t, db, ticket.ID)
		if got.state != string(StateConsumed) || got.version != 2 {
			t.Fatalf("第 %d 轮 %+v", i, got)
		}
		if code, ok := errorCode(errs[1]); !ok || code != "approval_mismatch" {
			t.Fatalf("第 %d 轮错误码 = %s ok=%v", i, code, ok)
		}
	}
}

func TestConsumeContextMismatch(t *testing.T) {
	ctx := t.Context()
	db := openApprovalDB(t)
	m := New(db)
	ticket := mustApprove(t, ctx, m, db, "ctx", []byte(`{"object":"contacts"}`))
	good := contextOf(ticket)
	upper := good
	upper.ArgsHash = upperHex(good.ArgsHash)
	cases := []struct {
		name string
		cc   ConsumeContext
	}{
		{name: "只有运行不对", cc: ConsumeContext{RunID: "nope", StepID: good.StepID, ArgsHash: good.ArgsHash}},
		{name: "只有步骤不对", cc: ConsumeContext{RunID: good.RunID, StepID: "nope", ArgsHash: good.ArgsHash}},
		{name: "只有哈希不对", cc: ConsumeContext{RunID: good.RunID, StepID: good.StepID, ArgsHash: upperHex(good.ArgsHash)}},
		{name: "哈希是大写", cc: upper},
		{name: "哈希是空字符串", cc: ConsumeContext{RunID: good.RunID, StepID: good.StepID, ArgsHash: ""}},
		{name: "零值不是通配", cc: ConsumeContext{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := readApproval(t, db, ticket.ID)
			audits := auditCount(t, db)
			err := m.Consume(ctx, ticket.ID, tc.cc)
			if !errors.Is(err, ErrMismatch) || errors.Is(err, ErrSnapshotMismatch) || errors.Is(err, ErrVersionConflict) {
				t.Fatalf("错误 = %v", err)
			}
			got := readApproval(t, db, ticket.ID)
			if got != before || auditCount(t, db) != audits {
				t.Fatalf("核对失败改了确认单或写了审计: %+v audits %d", got, auditCount(t, db))
			}
		})
	}
	if err := m.Consume(ctx, ticket.ID, good); err != nil {
		t.Fatal(err)
	}
	got := readApproval(t, db, ticket.ID)
	if got.state != string(StateConsumed) || got.version != 2 {
		t.Fatalf("核对失败之后正确消费 = %+v", got)
	}
}

func TestSnapshotTamper(t *testing.T) {
	ctx := t.Context()
	original := []byte(`{"b":1,"a":2}`)
	reordered := []byte(`{"a":2,"b":1}`)
	sum, err := ParamsHash(original)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParamsHash(reordered)
	if err != nil || again != sum {
		t.Fatalf("键序不同应得到同一哈希: %s %s %v", sum, again, err)
	}

	t.Run("键序变化仍可消费", func(t *testing.T) {
		db := openApprovalDB(t)
		m := New(db)
		ticket := mustApprove(t, ctx, m, db, "reorder", original)
		if _, err := db.ExecContext(ctx, `UPDATE approvals SET params_json = ? WHERE id = ?`, string(reordered), ticket.ID); err != nil {
			t.Fatal(err)
		}
		if err := m.Consume(ctx, ticket.ID, contextOf(ticket)); err != nil {
			t.Fatal(err)
		}
		if got := readApproval(t, db, ticket.ID); got.state != string(StateConsumed) {
			t.Fatalf("键序 %+v", got)
		}
	})

	t.Run("改值和加字段", func(t *testing.T) {
		for _, params := range [][]byte{[]byte(`{"a":2,"b":9}`), []byte(`{"a":2,"b":1,"c":0}`)} {
			db := openApprovalDB(t)
			m := New(db)
			ticket := mustApprove(t, ctx, m, db, "tamper-"+string(params), original)
			if _, err := db.ExecContext(ctx, `UPDATE approvals SET params_json = ? WHERE id = ?`, string(params), ticket.ID); err != nil {
				t.Fatal(err)
			}
			before := readApproval(t, db, ticket.ID)
			err := m.Consume(ctx, ticket.ID, contextOf(ticket))
			if !errors.Is(err, ErrSnapshotMismatch) || errors.Is(err, ErrMismatch) {
				t.Fatalf("篡改 %s 错误 = %v", params, err)
			}
			got := readApproval(t, db, ticket.ID)
			if got.state != before.state || got.version != before.version {
				t.Fatalf("篡改消费改了确认单: %+v", got)
			}
			if runState(t, db, ticket.RunID) != "failed" {
				t.Fatalf("运行状态 = %s", runState(t, db, ticket.RunID))
			}
			rows := auditsOf(t, db)
			if len(rows) != 1 || rows[0].action != "snapshot_mismatch" || containsPhone(rows[0].detail) {
				t.Fatalf("审计 %+v", rows)
			}
		}
	})

	t.Run("调用方哈希正确但库里的参数被换过", func(t *testing.T) {
		db := openApprovalDB(t)
		m := New(db)
		ticket := mustApprove(t, ctx, m, db, "json-only", []byte(`{"object":"contacts"}`))
		if _, err := db.ExecContext(ctx, `UPDATE approvals SET params_json = ? WHERE id = ?`, `{"object":"other"}`, ticket.ID); err != nil {
			t.Fatal(err)
		}
		err := m.Consume(ctx, ticket.ID, contextOf(ticket))
		if !errors.Is(err, ErrSnapshotMismatch) || errors.Is(err, ErrMismatch) {
			t.Fatalf("错误 = %v", err)
		}
	})
}

func TestTamperedStoredHashIsRejected(t *testing.T) {
	ctx := t.Context()
	db := openApprovalDB(t)
	m := New(db)
	ticket := mustApprove(t, ctx, m, db, "hash", []byte(`{"object":"contacts"}`))
	other, err := ParamsHash([]byte(`{"object":"other"}`))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		hash string
		cc   string
		want error
	}{
		{name: "不是十六进制", hash: "zzzz", cc: "zzzz", want: ErrSnapshotMismatch},
		{name: "另一份合法哈希", hash: other, cc: other, want: ErrSnapshotMismatch},
		{name: "大写哈希", hash: upperHex(ticket.ParamsHash), cc: upperHex(ticket.ParamsHash), want: ErrSnapshotMismatch},
		{name: "调用方仍拿原哈希", hash: other, cc: ticket.ParamsHash, want: ErrMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, `UPDATE approvals SET params_hash = ? WHERE id = ?`, tc.hash, ticket.ID); err != nil {
				t.Fatal(err)
			}
			cc := contextOf(ticket)
			cc.ArgsHash = tc.cc
			err := m.Consume(ctx, ticket.ID, cc)
			if !errors.Is(err, tc.want) {
				t.Fatalf("错误 = %v，想要 %v", err, tc.want)
			}
			got := readApproval(t, db, ticket.ID)
			if got.state != string(StateApproved) || got.version != 1 {
				t.Fatalf("哈希被篡改后 %+v", got)
			}
		})
	}
}

func TestDeadline(t *testing.T) {
	ctx := t.Context()
	exp := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)

	t.Run("恰好到期算过期", func(t *testing.T) {
		db := openApprovalDB(t)
		m := New(db)
		m.SetClock(func() time.Time { return exp.Add(-time.Second) })
		ticket := mustCreatePending(t, ctx, m, db, "edge", []byte(`{"object":"contacts"}`), exp)
		m.SetClock(func() time.Time { return exp })
		if err := m.Expire(ctx, ticket.ID); err != nil {
			t.Fatal(err)
		}
		if got := readApproval(t, db, ticket.ID); got.state != string(StateExpired) {
			t.Fatalf("边界 %+v", got)
		}
	})

	t.Run("到期前不能写成过期", func(t *testing.T) {
		db := openApprovalDB(t)
		m := New(db)
		m.SetClock(func() time.Time { return exp.Add(-time.Second) })
		ticket := mustCreatePending(t, ctx, m, db, "early", []byte(`{"object":"contacts"}`), exp)
		m.SetClock(func() time.Time { return exp.Add(-time.Nanosecond) })
		before := auditCount(t, db)
		err := m.Expire(ctx, ticket.ID)
		if !errors.Is(err, ErrNotDue) {
			t.Fatalf("未到期 = %v", err)
		}
		got := readApproval(t, db, ticket.ID)
		if got.state != string(StatePending) || got.version != 0 || auditCount(t, db) != before {
			t.Fatalf("未到期被改了 %+v", got)
		}
	})

	t.Run("到期的 pending 和 approved 不能再批准或消费", func(t *testing.T) {
		db := openApprovalDB(t)
		m := New(db)
		m.SetClock(func() time.Time { return exp.Add(-time.Minute) })
		pending := mustCreatePending(t, ctx, m, db, "due-p", []byte(`{"object":"contacts"}`), exp)
		approved := mustApprove(t, ctx, m, db, "due-a", []byte(`{"object":"contacts"}`))
		if _, err := db.ExecContext(ctx, `UPDATE approvals SET expires_at = ? WHERE id = ?`, exp.Format(time.RFC3339Nano), approved.ID); err != nil {
			t.Fatal(err)
		}
		m.SetClock(func() time.Time { return exp.Add(time.Second) })
		if err := m.Decide(ctx, pending.ID, Decision{Action: ActionApprove}); !errors.Is(err, ErrExpired) {
			t.Fatalf("到期批准 = %v", err)
		}
		if got := readApproval(t, db, pending.ID); got.state != string(StateExpired) || got.version != 1 {
			t.Fatalf("pending 到期 %+v", got)
		}
		if err := m.Consume(ctx, approved.ID, contextOf(approved)); !errors.Is(err, ErrExpired) {
			t.Fatalf("到期消费 = %v", err)
		}
		if got := readApproval(t, db, approved.ID); got.state != string(StateExpired) || got.consumed.Valid || got.version != 2 {
			t.Fatalf("approved 到期 %+v", got)
		}
		if err := m.Decide(ctx, pending.ID, Decision{Action: ActionApprove}); !errors.Is(err, ErrIllegalTransition) || !errors.Is(err, ErrTerminal) {
			t.Fatalf("过期后再批准 = %v", err)
		}
		if err := m.Consume(ctx, approved.ID, contextOf(approved)); !errors.Is(err, ErrIllegalTransition) {
			t.Fatalf("过期后再消费 = %v", err)
		}
	})

	t.Run("已消费过了期限也不变", func(t *testing.T) {
		db := openApprovalDB(t)
		m := New(db)
		m.SetClock(func() time.Time { return exp.Add(-time.Minute) })
		ticket := mustApprove(t, ctx, m, db, "kept", []byte(`{"object":"contacts"}`))
		if err := m.Consume(ctx, ticket.ID, contextOf(ticket)); err != nil {
			t.Fatal(err)
		}
		before := readApproval(t, db, ticket.ID)
		m.SetClock(func() time.Time { return exp.Add(time.Hour) })
		if err := m.Expire(ctx, ticket.ID); !errors.Is(err, ErrTerminal) {
			t.Fatalf("消费后过期 = %v", err)
		}
		got := readApproval(t, db, ticket.ID)
		if got.state != string(StateConsumed) || got.version != before.version {
			t.Fatalf("已消费被改了 %+v", got)
		}
	})

	t.Run("空的和无法解析的期限都不能用", func(t *testing.T) {
		for _, expires := range []string{"", "yesterday"} {
			db := openApprovalDB(t)
			m := New(db)
			ticket := mustApprove(t, ctx, m, db, "bad-exp-"+expires, []byte(`{"object":"contacts"}`))
			if _, err := db.ExecContext(ctx, `UPDATE approvals SET expires_at = ? WHERE id = ?`, expires, ticket.ID); err != nil {
				t.Fatal(err)
			}
			err := m.Consume(ctx, ticket.ID, contextOf(ticket))
			if !errors.Is(err, ErrExpired) {
				t.Fatalf("期限 %q 消费 = %v", expires, err)
			}
			got := readApproval(t, db, ticket.ID)
			if got.state != string(StateExpired) || got.consumed.Valid {
				t.Fatalf("期限 %q 之后 %+v", expires, got)
			}
		}
	})
}

func TestConsumeAndExpireAtDeadline(t *testing.T) {
	ctx := t.Context()
	exp := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	db := openApprovalDB(t)
	m := New(db)
	m.SetClock(func() time.Time { return exp.Add(-time.Minute) })
	ticket := mustApprove(t, ctx, m, db, "race-due", []byte(`{"object":"contacts"}`))
	if _, err := db.ExecContext(ctx, `UPDATE approvals SET expires_at = ? WHERE id = ?`, exp.Format(time.RFC3339Nano), ticket.ID); err != nil {
		t.Fatal(err)
	}
	m.SetClock(func() time.Time { return exp })
	g := newPairGate()
	m.SetGate(g.enter)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = m.Consume(ctx, ticket.ID, contextOf(ticket))
	}()
	go func() {
		defer wg.Done()
		errs[1] = m.Expire(ctx, ticket.ID)
	}()
	wg.Wait()
	if g.arrived() != 2 {
		t.Fatal("到期竞争没有同时开始")
	}
	// 成功的那次是过期转移：Consume 返回 ErrExpired，Expire 返回 nil。失败者是版本冲突。
	var expiredWin, conflict int
	for _, err := range errs {
		switch {
		case err == nil || (errors.Is(err, ErrExpired) && !errors.Is(err, ErrVersionConflict)):
			expiredWin++
		case errors.Is(err, ErrVersionConflict):
			conflict++
		default:
			t.Fatalf("到期竞争错误 = %v", err)
		}
	}
	if expiredWin != 1 || conflict != 1 {
		t.Fatalf("到期竞争结果 %v", errs)
	}
	got := readApproval(t, db, ticket.ID)
	if got.state != string(StateExpired) || got.consumed.Valid || got.version != 2 {
		t.Fatalf("到期竞争后 %+v", got)
	}
}

func TestApproveAndExpireRace(t *testing.T) {
	ctx := t.Context()
	exp := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	db := openApprovalDB(t)
	m := New(db)
	m.SetClock(func() time.Time { return exp })
	ticket := mustCreatePending(t, ctx, m, db, "race-ap", []byte(`{"object":"contacts"}`), exp)
	g := newPairGate()
	m.SetGate(g.enter)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = m.Decide(ctx, ticket.ID, Decision{Action: ActionApprove})
	}()
	go func() {
		defer wg.Done()
		errs[1] = m.Expire(ctx, ticket.ID)
	}()
	wg.Wait()
	if g.arrived() != 2 {
		t.Fatal("批准和过期没有同时开始")
	}
	var win, conflict int
	for _, err := range errs {
		switch {
		case err == nil || (errors.Is(err, ErrExpired) && !errors.Is(err, ErrVersionConflict)):
			win++
		case errors.Is(err, ErrVersionConflict):
			conflict++
		default:
			t.Fatalf("竞争错误 = %v / %v", errs[0], errs[1])
		}
	}
	if win != 1 || conflict != 1 {
		t.Fatalf("竞争结果 %v", errs)
	}
	got := readApproval(t, db, ticket.ID)
	if got.state != string(StateExpired) || got.version != 1 || got.consumed.Valid {
		t.Fatalf("竞争后 %+v", got)
	}
}

func TestDeferAndUnknownDecision(t *testing.T) {
	ctx := t.Context()
	exp := time.Date(2026, 10, 21, 11, 0, 0, 0, time.UTC)
	db := openApprovalDB(t)
	m := New(db)
	m.SetClock(func() time.Time { return exp.Add(time.Hour) })
	ticket := mustCreatePending(t, ctx, m, db, "defer", []byte(`{"object":"contacts"}`), exp)
	before := auditCount(t, db)
	if err := m.Decide(ctx, ticket.ID, Decision{Action: ActionDefer}); err != nil {
		t.Fatal(err)
	}
	got := readApproval(t, db, ticket.ID)
	if got.state != string(StatePending) || got.version != 0 || got.decision != "" || auditCount(t, db) != before {
		t.Fatalf("暂缓 %+v", got)
	}
	err := m.Decide(ctx, ticket.ID, Decision{Action: "nope"})
	if !errors.Is(err, ErrInvalidDecision) {
		t.Fatalf("未知决定 = %v", err)
	}
	if auditCount(t, db) != before || readApproval(t, db, ticket.ID) != got {
		t.Fatal("未知决定写了库")
	}
}

func TestCorruptDecisionDoesNotConsume(t *testing.T) {
	ctx := t.Context()
	db := openApprovalDB(t)
	m := New(db)
	ticket := mustCreatePending(t, ctx, m, db, "corrupt", []byte(`{"phone":"13800138000"}`), time.Now().Add(time.Hour))
	if _, err := db.ExecContext(ctx, `UPDATE approvals SET decision = 'approve' WHERE id = ?`, ticket.ID); err != nil {
		t.Fatal(err)
	}
	err := m.Consume(ctx, ticket.ID, contextOf(ticket))
	if !errors.Is(err, ErrCorrupt) || errors.Is(err, ErrMismatch) {
		t.Fatalf("损坏 = %v", err)
	}
	got := readApproval(t, db, ticket.ID)
	if got.state != string(StatePending) || got.version != 0 {
		t.Fatalf("损坏行被消费 %+v", got)
	}
	rows := auditsOf(t, db)
	if len(rows) != 1 || rows[0].action != "corrupt" || containsPhone(rows[0].detail) {
		t.Fatalf("审计 %+v", rows)
	}
}

func TestDifferentReplacementIsNotApplied(t *testing.T) {
	ctx := t.Context()
	db := openApprovalDB(t)
	m := New(db)
	original := []byte(`{"object":"contacts"}`)
	ticket := mustCreatePending(t, ctx, m, db, "swap", original, time.Now().Add(time.Hour))
	err := m.Decide(ctx, ticket.ID, Decision{Action: ActionApprove, Params: []byte(`{"object":"other"}`)})
	if !errors.Is(err, ErrSnapshotMismatch) {
		t.Fatalf("替换参数 = %v", err)
	}
	got := readApproval(t, db, ticket.ID)
	if got.state != string(StatePending) || got.version != 0 || got.decision != "" {
		t.Fatalf("不同参数仍写了决定 %+v", got)
	}
	if runState(t, db, ticket.RunID) != "failed" {
		t.Fatalf("运行 = %s", runState(t, db, ticket.RunID))
	}
	same, err := ParamsHash(original)
	if err != nil {
		t.Fatal(err)
	}
	spaced := []byte(`{ "object" : "contacts" }`)
	if gotHash, err := ParamsHash(spaced); err != nil || gotHash != same {
		t.Fatalf("空白应同一哈希 %s %v", gotHash, err)
	}
	if err := m.Decide(ctx, ticket.ID, Decision{Action: ActionApprove, Params: spaced}); err != nil {
		t.Fatal(err)
	}
	got = readApproval(t, db, ticket.ID)
	if got.state != string(StateApproved) || string(mustParams(t, db, ticket.ID)) != string(original) {
		t.Fatalf("同一哈希应沿用原参数 %+v", got)
	}
}

func TestScopeCheckedBeforeExpiry(t *testing.T) {
	ctx := t.Context()
	db := openApprovalDB(t)
	m := New(db)
	exp := time.Now().Add(-time.Minute).UTC()
	ticket := mustApprove(t, ctx, m, db, "scope", []byte(`{"object":"contacts"}`))
	if _, err := db.ExecContext(ctx, `UPDATE approvals SET expires_at = ? WHERE id = ?`, exp.Format(time.RFC3339Nano), ticket.ID); err != nil {
		t.Fatal(err)
	}
	err := m.ConsumeForExport(ctx, ticket.ID, contextOf(ticket), ticket.WorkspaceID, "other-object")
	if !errors.Is(err, ErrScope) || errors.Is(err, ErrExpired) {
		t.Fatalf("对象不对 = %v", err)
	}
	got := readApproval(t, db, ticket.ID)
	if got.state != string(StateApproved) || got.version != 1 {
		t.Fatalf("对象不对却过期或消费了 %+v", got)
	}
	err = m.ConsumeForExport(ctx, ticket.ID, contextOf(ticket), "other-ws", ticket.ExportObject)
	if !errors.Is(err, ErrScope) {
		t.Fatalf("工作区不对 = %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE approvals SET action = 'write_action' WHERE id = ?`, ticket.ID); err != nil {
		t.Fatal(err)
	}
	err = m.ConsumeForExport(ctx, ticket.ID, contextOf(ticket), ticket.WorkspaceID, ticket.ExportObject)
	if !errors.Is(err, ErrScope) {
		t.Fatalf("动作不对 = %v", err)
	}
	if readApproval(t, db, ticket.ID).state != string(StateApproved) {
		t.Fatal("动作不对却改了状态")
	}
}

func assertPublicIllegal(t *testing.T, ctx context.Context, params []byte, id string, setup func(context.Context, *Machine, *sql.DB, Ticket), act func(context.Context, *Machine, Ticket) error, state State, terminalState bool) {
	t.Helper()
	db := openApprovalDB(t)
	m := New(db)
	ticket := mustCreatePending(t, ctx, m, db, id, params, time.Now().Add(time.Hour))
	setup(ctx, m, db, ticket)
	before := readApproval(t, db, ticket.ID)
	audits := auditCount(t, db)
	err := act(ctx, m, ticket)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("错误 = %v", err)
	}
	if terminalState && !errors.Is(err, ErrTerminal) {
		t.Fatalf("终态错误 = %v", err)
	}
	if !terminalState && errors.Is(err, ErrTerminal) {
		t.Fatalf("不该是终态错误 = %v", err)
	}
	got := readApproval(t, db, ticket.ID)
	if got.state != string(state) || got.version != before.version || got.decided != before.decided || got.consumed != before.consumed {
		t.Fatalf("非法转移改了行 before=%+v after=%+v", before, got)
	}
	if auditCount(t, db) != audits+1 {
		t.Fatalf("审计 %d → %d", audits, auditCount(t, db))
	}
	rows := auditsOf(t, db)
	if containsPhone(rows[len(rows)-1].detail) || containsPhone(err.Error()) {
		t.Fatalf("带了手机号 %q %v", rows[len(rows)-1].detail, err)
	}
}

type approvalSnap struct {
	state    string
	version  int64
	decision string
	decided  sql.NullString
	consumed sql.NullString
}

func openApprovalDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runs.db")
	dsn := "file:" + filepath.ToSlash(path) + "?_busy_timeout=5000&_foreign_keys=on&_journal_mode=WAL"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(8)
	t.Cleanup(func() { _ = db.Close() })
	root := moduleRoot(t)
	for _, name := range []string{"001_init.sql", "002_export_approval.sql", "003_approval_state.sql"} {
		file, err := filepath.EvalSymlinks(filepath.Join(root, "core", "workspace", "migrations", "runs", name))
		if err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("迁移 %s: %v", name, err)
		}
	}
	return db
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("找不到测试文件")
	}
	dir := filepath.Dir(file)
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.HasPrefix(string(data), "module github.com/loveRyujin/lantern\n") {
			resolved, err := filepath.EvalSymlinks(dir)
			if err != nil {
				t.Fatal(err)
			}
			return resolved
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("找不到模块根")
		}
		dir = parent
	}
}

func mustCreatePending(t *testing.T, ctx context.Context, m *Machine, db *sql.DB, id string, params []byte, exp time.Time) Ticket {
	t.Helper()
	hash, err := ParamsHash(params)
	if err != nil {
		t.Fatal(err)
	}
	ticket := Ticket{
		ID:           id,
		RunID:        "run-" + id,
		StepID:       "step-" + id,
		WorkspaceID:  "ws",
		Action:       "export_data",
		ExportObject: "contacts",
		Params:       append([]byte(nil), params...),
		ParamsHash:   hash,
		ExpiresAt:    exp,
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO runs (id, state, version, context, created_at, updated_at)
		VALUES (?, 'running', 0, '', ?, ?)`, ticket.RunID, now, now); err != nil {
		t.Fatal(err)
	}
	if err := m.Create(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	return ticket
}

func mustApprove(t *testing.T, ctx context.Context, m *Machine, db *sql.DB, id string, params []byte) Ticket {
	t.Helper()
	// 期限放得很远，测试把时钟拨到 2026 年时不会误判成已经到期。
	ticket := mustCreatePending(t, ctx, m, db, id, params, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
	if err := m.Decide(ctx, ticket.ID, Decision{Action: ActionApprove}); err != nil {
		t.Fatal(err)
	}
	return ticket
}

func contextOf(ticket Ticket) ConsumeContext {
	return ConsumeContext{RunID: ticket.RunID, StepID: ticket.StepID, ArgsHash: ticket.ParamsHash}
}

func readApproval(t *testing.T, db *sql.DB, id string) approvalSnap {
	t.Helper()
	var got approvalSnap
	err := db.QueryRow(`
		SELECT state, version, decision, decided_at, consumed_at FROM approvals WHERE id = ?`, id).
		Scan(&got.state, &got.version, &got.decision, &got.decided, &got.consumed)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func runState(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var state string
	if err := db.QueryRow(`SELECT state FROM runs WHERE id = ?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func mustParams(t *testing.T, db *sql.DB, id string) []byte {
	t.Helper()
	var raw string
	if err := db.QueryRow(`SELECT params_json FROM approvals WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return []byte(raw)
}

type auditRow struct {
	action string
	detail string
}

func auditsOf(t *testing.T, db *sql.DB) []auditRow {
	t.Helper()
	rows, err := db.Query(`SELECT action, detail FROM audit ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var row auditRow
		if err := rows.Scan(&row.action, &row.detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func auditCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	return len(auditsOf(t, db))
}

func containsPhone(s string) bool {
	return strings.Contains(s, "13800138000") || strings.Contains(s, "phone")
}

func upperHex(h string) string {
	out := []byte(h)
	for i, c := range out {
		if c >= 'a' && c <= 'f' {
			out[i] = c - 'a' + 'A'
		}
	}
	return string(out)
}

func errorCode(err error) (string, bool) {
	code, ok := errcode.CodeOf(err)
	return string(code), ok
}

type pairGate struct {
	mu     sync.Mutex
	n      int
	ch     chan struct{}
	once   sync.Once
	cancel context.CancelFunc
}

func newPairGate() *pairGate {
	g := &pairGate{ch: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	g.cancel = cancel
	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			g.release()
		}
	}()
	return g
}

func (g *pairGate) enter() {
	g.mu.Lock()
	g.n++
	if g.n >= 2 {
		g.release()
	}
	g.mu.Unlock()
	<-g.ch
}

func (g *pairGate) release() {
	g.once.Do(func() { close(g.ch) })
}

func (g *pairGate) arrived() int {
	g.cancel()
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n
}
