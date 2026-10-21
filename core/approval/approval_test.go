package approval_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
)

func TestParamsHashCanonical(t *testing.T) {
	left := []byte(" { \"b\" : 1, \"a\" : \"云\", \"nested\" : { \"z\" : true, \"y\" : [1, 2] } } ")
	right := []byte(`{"a":"云","b":1,"nested":{"y":[1,2],"z":true}}`)
	lh, err := approval.ParamsHash(left)
	if err != nil {
		t.Fatal(err)
	}
	rh, err := approval.ParamsHash(right)
	if err != nil {
		t.Fatal(err)
	}
	if lh != rh || len(lh) != 64 {
		t.Fatalf("哈希不一致或长度不对: %s vs %s", lh, rh)
	}
	html, err := approval.ParamsHash([]byte(`{"q":"a<b&c>"}`))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(`{"q":"a<b&c>"}`))
	if html != hex.EncodeToString(sum[:]) {
		t.Fatalf("规范化把 & < > 转义了: %s", html)
	}
	reordered, err := approval.ParamsHash([]byte(`{"nested":{"y":[2,1]}}`))
	if err != nil {
		t.Fatal(err)
	}
	sameKeys, err := approval.ParamsHash([]byte(`{"nested":{"y":[1,2]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if reordered == sameKeys {
		t.Fatal("数组顺序不同却得到了相同哈希")
	}
	if _, err := approval.ParamsHash([]byte(`{"a":1}{"b":2}`)); !errors.Is(err, approval.ErrInvalidParams) {
		t.Fatalf("多个 JSON 值 err=%v", err)
	}
	if _, err := approval.ParamsHash([]byte("")); !errors.Is(err, approval.ErrInvalidParams) {
		t.Fatal(err)
	}
	one, err := approval.ParamsHash([]byte(`{"n":1}`))
	if err != nil {
		t.Fatal(err)
	}
	onePoint, err := approval.ParamsHash([]byte(`{"n":1.0}`))
	if err != nil {
		t.Fatal(err)
	}
	if one == onePoint {
		t.Fatal("数字写法不同应该得到不同哈希")
	}
}

func TestParamsHashEmptyArrayIsNotNull(t *testing.T) {
	empty, err := approval.ParamsHash([]byte("[]"))
	if err != nil {
		t.Fatal(err)
	}
	null, err := approval.ParamsHash([]byte("null"))
	if err != nil {
		t.Fatal(err)
	}
	if empty == null {
		t.Fatal(`ParamsHash("[]") == ParamsHash("null")`)
	}
	emptySum := sha256.Sum256([]byte("[]"))
	nullSum := sha256.Sum256([]byte("null"))
	if empty != hex.EncodeToString(emptySum[:]) || null != hex.EncodeToString(nullSum[:]) {
		t.Fatalf("空数组或 null 没有按原样规范化: %s vs %s", empty, null)
	}
}

func TestParamsHashRejectsDuplicateKeys(t *testing.T) {
	cases := []string{
		`{"rows":1,"rows":100000}`,
		`{"a":1,"b":{"rows":1,"rows":2}}`,
		`[{"rows":1,"rows":2}]`,
		`{"\u0072ows":1,"rows":100000}`,
	}
	for _, raw := range cases {
		if _, err := approval.ParamsHash([]byte(raw)); !errors.Is(err, approval.ErrInvalidParams) {
			t.Fatalf("%s err=%v，想要拒绝重复键", raw, err)
		}
	}
	if _, err := approval.ParamsHash([]byte(`{"a":{"x":1},"b":{"x":2}}`)); err != nil {
		t.Fatalf("不同对象里的同名键应该允许: %v", err)
	}
}

func TestTicketSnapshot(t *testing.T) {
	ticket := mustTicket(t)
	if err := ticket.VerifySnapshot(); err != nil {
		t.Fatal(err)
	}
	ticket.Params = []byte(`{"rows":3}`)
	if !errors.Is(ticket.VerifySnapshot(), approval.ErrSnapshotMismatch) {
		t.Fatal("篡改参数后应该哈希不一致")
	}
	ok, err := approval.SnapshotMatches(mustTicket(t).ParamsHash, mustTicket(t).Params)
	if err != nil || !ok {
		t.Fatalf("快照匹配 = %v %v", ok, err)
	}
}

func TestFakeDecisions(t *testing.T) {
	ticket := mustTicket(t)
	tampered := []byte(`{"rows":99,"tampered":true}`)
	fake := &approval.Fake{Responses: []approval.Response{
		{Action: approval.ActionApprove},
		{Action: approval.ActionReject},
		{Action: approval.ActionDefer},
		{Action: approval.ActionApprove, Params: tampered},
	}}
	ctx := context.Background()

	approved, err := fake.Request(ctx, ticket)
	if err != nil || approved.Action != approval.ActionApprove || approved.OverridesSnapshot() {
		t.Fatalf("批准 = %+v %v", approved, err)
	}
	rejected, err := fake.Request(ctx, ticket)
	if err != nil || rejected.Action != approval.ActionReject {
		t.Fatalf("拒绝 = %+v %v", rejected, err)
	}
	deferred, err := fake.Request(ctx, ticket)
	if err != nil || deferred.Action != approval.ActionDefer {
		t.Fatalf("Escape 应记为暂缓: %+v %v", deferred, err)
	}
	changed, err := fake.Request(ctx, ticket)
	if err != nil || !changed.OverridesSnapshot() {
		t.Fatalf("篡改 = %+v %v", changed, err)
	}
	ok, err := approval.SnapshotMatches(ticket.ParamsHash, changed.Params)
	if err != nil || ok {
		t.Fatal("替换参数不应该通过原哈希")
	}
	ok, err = approval.SnapshotMatches(ticket.ParamsHash, ticket.Params)
	if err != nil || !ok {
		t.Fatal("确认单原参数应该仍与哈希一致")
	}
	if _, err := fake.Request(ctx, ticket); !errors.Is(err, approval.ErrNoScript) {
		t.Fatalf("预设用尽 err=%v", err)
	}
	if got := len(fake.Tickets()); got != 5 {
		t.Fatalf("记下的确认单 = %d", got)
	}
}

func TestEmptyParamsAreNotOverride(t *testing.T) {
	empty := approval.Decision{Action: approval.ActionApprove, Params: []byte{}}
	if err := empty.Validate(); err != nil {
		t.Fatal(err)
	}
	if empty.OverridesSnapshot() {
		t.Fatal("空切片应视为没有替换参数")
	}
	if (approval.Decision{Action: approval.ActionApprove}).OverridesSnapshot() {
		t.Fatal("nil 参数应视为没有替换")
	}
}

func TestApproveReplacementFailsVerifySnapshot(t *testing.T) {
	ticket := mustTicket(t)
	replaced := []byte(`{"rows":100000}`)
	decision := approval.Decision{Action: approval.ActionApprove, Params: replaced}
	if err := decision.Validate(); err != nil {
		t.Fatal(err)
	}
	if !decision.OverridesSnapshot() {
		t.Fatal("非空替换参数应该被识别")
	}
	forged := ticket
	forged.Params = decision.Params
	if err := forged.VerifySnapshot(); !errors.Is(err, approval.ErrSnapshotMismatch) {
		t.Fatalf("替换后的参数跑 VerifySnapshot err=%v", err)
	}
	if err := ticket.VerifySnapshot(); err != nil {
		t.Fatalf("原确认单被改写了: %v", err)
	}
}

func TestFakeSilenceWaitsForContext(t *testing.T) {
	fake := &approval.Fake{Responses: []approval.Response{{Wait: true}}}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := fake.Request(ctx, mustTicket(t))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("不响应 err=%v", err)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("不响应提前返回了")
	}
}

func TestFakeDoesNotConsumeScriptForInvalidTicket(t *testing.T) {
	fake := &approval.Fake{Responses: []approval.Response{{Action: approval.ActionApprove}}}
	if _, err := fake.Request(context.Background(), approval.Ticket{}); !errors.Is(err, approval.ErrInvalidTicket) {
		t.Fatal(err)
	}
	decision, err := fake.Request(context.Background(), mustTicket(t))
	if err != nil || decision.Action != approval.ActionApprove {
		t.Fatalf("非法确认单消耗了预设: %+v %v", decision, err)
	}
}

func mustTicket(t *testing.T) approval.Ticket {
	t.Helper()
	params := []byte(`{"action":"export_data","rows":2}`)
	sum, err := approval.ParamsHash(params)
	if err != nil {
		t.Fatal(err)
	}
	return approval.Ticket{
		ID:            "ap1",
		RunID:         "run1",
		WorkspaceID:   "xinghe",
		Prompt:        "导出用户",
		Action:        "export_data",
		Params:        params,
		ParamsHash:    sum,
		Sensitive:     []string{"phone"},
		EstimatedRows: 2,
		Risk:          "含敏感字段",
	}
}
