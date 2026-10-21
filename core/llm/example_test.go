package llm_test

import (
	"context"
	"fmt"

	"github.com/loveRyujin/lantern/core/llm"
)

func ExampleMock_Chat() {
	script := llm.MustNewScript(
		llm.CallTools(llm.ToolCall{ID: "call_1", Name: "run_sql", Arguments: `{"query":"SELECT 1"}`}),
		llm.Say("结果是 ", "1。"),
	)
	mock := llm.NewMock(script)
	ctx := context.Background()
	first, err := mock.Chat(ctx, llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "算一下"}},
		Tools:    []llm.Tool{{Name: "run_sql"}},
	})
	if err != nil {
		panic(err)
	}
	call := first.Message.ToolCalls[0]
	fmt.Printf("%s %s\n", call.Name, call.Arguments)

	second, err := mock.Chat(ctx, llm.Request{
		Stream: true,
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "算一下"},
			{Role: llm.RoleAssistant, ToolCalls: first.Message.ToolCalls},
			{Role: llm.RoleTool, ToolCallID: call.ID, Content: "1"},
		},
		OnDelta: func(d llm.Delta) error {
			fmt.Print(d.Content)
			return nil
		},
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("\n%s\n", second.FinishReason)
	// Output:
	// run_sql {"query":"SELECT 1"}
	// 结果是 1。
	// stop
}
