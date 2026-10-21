package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func echoSpec() Spec {
	return Spec{
		Name:        "echo",
		Description: "回声",
		Parameters: json.RawMessage(`{
			"type": "object",
			"additionalProperties": false,
			"required": ["q"],
			"properties": {
				"q": {"type": "string"},
				"limit": {"type": "integer", "minimum": 1},
				"filter": {
					"type": "object",
					"additionalProperties": false,
					"properties": {"k": {"type": "string"}}
				},
				"ids": {"type": "array", "items": {"type": "integer"}}
			}
		}`),
	}
}

type echoTool struct{ spec Spec }

func (e echoTool) Spec() Spec { return e.spec }

func (e echoTool) Execute(ctx context.Context, call Call) (Result, error) {
	return Result{Content: "ok", Summary: "已完成"}, nil
}

func newEcho(t *testing.T) *Registry {
	t.Helper()
	reg := &Registry{}
	if err := reg.Register(echoTool{spec: echoSpec()}); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestPrepareTruncationSkipsParse(t *testing.T) {
	orig := parseJSON
	t.Cleanup(func() { parseJSON = orig })
	var calls int
	parseJSON = func(raw []byte) (any, error) {
		calls++
		return orig(raw)
	}
	reg := newEcho(t)
	for _, in := range []CallInput{
		{Name: "echo", Arguments: `{"q":"ok"}`, FinishReason: "length"},
		{Name: "echo", Arguments: `{"q":`, FinishReason: "length"},
		{Name: "echo", Arguments: `{"q":"ok"}`, FinishReason: "content_filter"},
		{Name: "echo", Arguments: `{"q":"ok"}`, Truncated: true, FinishReason: "stop"},
	} {
		calls = 0
		_, err := reg.Prepare(in)
		if calls != 0 {
			t.Fatalf("截断路径解析了参数 %+v", in)
		}
		code, ok := CodeOf(err)
		if !ok || code != CodeArgumentsIncomplete || !ArgumentFailure(err) {
			t.Fatalf("截断错误 = %v", err)
		}
	}
	calls = 0
	_, err := reg.Prepare(CallInput{Name: "echo", Arguments: `{"q":`, FinishReason: "stop"})
	if calls != 1 {
		t.Fatalf("非法 JSON 应解析一次，实际 %d 次", calls)
	}
	if code, _ := CodeOf(err); code != CodeArgumentsInvalid {
		t.Fatalf("半截 JSON = %v", err)
	}
}

func TestPrepareInvalidJSON(t *testing.T) {
	reg := newEcho(t)
	for _, args := range []string{
		``,
		`   `,
		`{"q":`,
		`{"q":"a"}{"q":"b"}`,
		`{"q":"a"} trailing`,
		`{"q":"a","q":"b"}`,
		`{"q":"a","filter":{"k":"1","k":"2"}}`,
		`[]`,
		`"hi"`,
		`1`,
		`null`,
		`true`,
	} {
		_, err := reg.Prepare(CallInput{Name: "echo", Arguments: args, FinishReason: "tool_calls"})
		if code, ok := CodeOf(err); !ok || code != CodeArgumentsInvalid || !ArgumentFailure(err) {
			t.Fatalf("参数 %q 的错误 = %v", args, err)
		}
	}
}

func TestPrepareSchemaMismatch(t *testing.T) {
	reg := newEcho(t)
	for _, args := range []string{
		`{}`,
		`{"q":1}`,
		`{"q":"a","nope":1}`,
		`{"q":"a","filter":{"k":"b","z":1}}`,
		`{"q":"a","ids":[1,"x"]}`,
		`{"q":"a","requires_approval":false,"approved":true}`,
		`{"q":"a","limit":0}`,
	} {
		_, err := reg.Prepare(CallInput{Name: "echo", Arguments: args})
		if code, ok := CodeOf(err); !ok || code != CodeArgumentsInvalid {
			t.Fatalf("参数 %s 的错误 = %v", args, err)
		}
		if strings.Contains(err.Error(), args) && strings.Contains(args, "requires_approval") {
			t.Fatalf("错误回显了参数: %v", err)
		}
	}
	got, err := reg.Prepare(CallInput{Name: "echo", Arguments: `{"q":"a","limit":2,"ids":[1,2]}`})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "echo" || got.Hash == "" {
		t.Fatalf("合法参数没有准备好: %+v", got)
	}
}

func TestPrepareSizeDepthUTF8AndNumbers(t *testing.T) {
	orig := parseJSON
	t.Cleanup(func() { parseJSON = orig })
	var calls int
	parseJSON = func(raw []byte) (any, error) {
		calls++
		return orig(raw)
	}
	reg := newEcho(t)
	_, err := reg.Prepare(CallInput{Name: "echo", Arguments: strings.Repeat("a", MaxArgumentBytes+1)})
	if calls != 0 {
		t.Fatal("超限参数不应解析")
	}
	if code, _ := CodeOf(err); code != CodeArgumentsInvalid {
		t.Fatalf("超限 = %v", err)
	}

	_, err = reg.Prepare(CallInput{Name: "echo", Arguments: nestedObject(10000)})
	if code, _ := CodeOf(err); code != CodeArgumentsInvalid {
		t.Fatalf("深层嵌套 = %v", err)
	}

	_, err = reg.Prepare(CallInput{Name: "echo", Arguments: string([]byte{0xff, 0xfe})})
	if code, _ := CodeOf(err); code != CodeArgumentsInvalid {
		t.Fatalf("非法 UTF-8 = %v", err)
	}
	if _, err = reg.Prepare(CallInput{Name: "echo", Arguments: "{\"q\":\"a\\u0000b\"}"}); err != nil {
		t.Fatalf(`\u0000 不应崩溃: %v`, err)
	}
	if _, err = reg.Prepare(CallInput{Name: "echo", Arguments: "{\"q\":\"a\x00b\"}"}); err == nil {
		t.Fatal("字符串里未转义的 NUL 不是合法 JSON")
	}

	for _, args := range []string{
		`{"q":"a","limit":1e400}`,
		`{"q":"a","limit":1e999999999999}`,
		`{"q":"a","limit":-0}`,
		`{"q":"a","limit":` + strings.Repeat("9", 80) + `}`,
	} {
		first, err1 := reg.Prepare(CallInput{Name: "echo", Arguments: args})
		second, err2 := reg.Prepare(CallInput{Name: "echo", Arguments: args})
		if (err1 == nil) != (err2 == nil) {
			t.Fatalf("%s 两次结果不一致: %v %v", args, err1, err2)
		}
		if err1 != nil && err1.Error() != err2.Error() {
			t.Fatalf("%s 错误不稳定: %v %v", args, err1, err2)
		}
		if err1 == nil && first.Hash != second.Hash {
			t.Fatalf("%s 哈希不稳定", args)
		}
	}
	_, err = reg.Prepare(CallInput{Name: "echo", Arguments: `{"q":"a","limit":1e999999999999}`})
	if code, _ := CodeOf(err); code != CodeArgumentsInvalid {
		t.Fatalf("无法表示的数字 = %v", err)
	}
}

func TestPrepareUnknownToolExactName(t *testing.T) {
	reg := newEcho(t)
	for _, name := range []string{"Echo", "echo ", " echo", "echo\n"} {
		_, err := reg.Prepare(CallInput{Name: name, Arguments: `{"q":"a"}`})
		code, ok := CodeOf(err)
		if !ok || code != CodeToolNotFound || ArgumentFailure(err) {
			t.Fatalf("名字 %q 的错误 = %v", name, err)
		}
		var te *Error
		if !errorAs(err, &te) || !te.Retryable {
			t.Fatalf("未知工具应可重试: %v", err)
		}
	}
	if _, err := reg.Prepare(CallInput{Name: "echo", Arguments: `{"q":"a"}`}); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterDuplicateDoesNotOverwrite(t *testing.T) {
	reg := &Registry{}
	first := echoSpec()
	first.Description = "第一份"
	second := echoSpec()
	second.Description = "第二份"
	if err := reg.Register(echoTool{spec: first}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(echoTool{spec: second}); err == nil {
		t.Fatal("同名注册应失败")
	}
	specs := reg.Specs()
	if len(specs) != 1 || specs[0].Description != "第一份" {
		t.Fatalf("注册表被覆盖: %+v", specs)
	}
	bad := Spec{Name: "bad", Parameters: json.RawMessage(`{"type":`)}
	if err := reg.Register(echoTool{spec: bad}); err == nil {
		t.Fatal("坏 Schema 应拒绝注册")
	}
	if len(reg.Specs()) != 1 {
		t.Fatal("坏 Schema 进入了注册表")
	}
}

func TestRegisterConcurrent(t *testing.T) {
	reg := &Registry{}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- reg.Register(echoTool{spec: echoSpec()})
		}()
	}
	wg.Wait()
	close(errs)
	okN := 0
	for err := range errs {
		if err == nil {
			okN++
		}
	}
	if okN != 1 || len(reg.Specs()) != 1 {
		t.Fatalf("并发注册成功 %d 次，表里有 %d 个", okN, len(reg.Specs()))
	}
}

func TestRegisterBuiltins(t *testing.T) {
	reg := &Registry{}
	if err := RegisterBuiltins(reg); err != nil {
		t.Fatal(err)
	}
	got := reg.Specs()
	want := []string{"export_data", "run_python", "run_sql", "search_docs", "write_action"}
	if len(got) != len(want) {
		t.Fatalf("桩数量 %d", len(got))
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Fatalf("顺序 = %s，想要 %s", got[i].Name, name)
		}
	}
	if err := RegisterBuiltins(reg); err == nil {
		t.Fatal("重复登记桩应失败")
	}
	if len(reg.Specs()) != len(want) {
		t.Fatal("失败的重复登记改动了注册表")
	}
	_, err := reg.Prepare(CallInput{Name: "run_sql", Arguments: `{"limit":1}`})
	if code, _ := CodeOf(err); code != CodeArgumentsInvalid {
		t.Fatalf("缺少 sql = %v", err)
	}
	prepared, err := reg.Prepare(CallInput{Name: "run_sql", Arguments: `{"sql":"select 1"}`})
	if err != nil || prepared.Name != "run_sql" {
		t.Fatalf("合法 run_sql = %v %+v", err, prepared)
	}
	if _, err = reg.Prepare(CallInput{Name: "run_sql", Arguments: `{"sql":"select 1","approved":true}`}); err == nil {
		t.Fatal("模型塞进的额外字段应被 Schema 拒绝")
	}
}

func TestSkipIsNotArgumentFailure(t *testing.T) {
	err := Skip()
	if ArgumentFailure(err) || CodeArgumentsIncomplete == "tool_args_truncated" {
		t.Fatal("跳过不应计入参数失败，也不使用已作废的错误码")
	}
	if code, _ := CodeOf(err); code != CodeCallSkipped {
		t.Fatal(err)
	}
}

func nestedObject(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(`{"a":`)
	}
	b.WriteByte('1')
	for i := 0; i < n; i++ {
		b.WriteByte('}')
	}
	return b.String()
}

func errorAs(err error, target **Error) bool {
	if err == nil {
		return false
	}
	e, ok := err.(*Error)
	if !ok {
		return false
	}
	*target = e
	return true
}
