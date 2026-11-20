package agent

import (
	"strings"
	"testing"

	"github.com/loveRyujin/lantern/core/llm"
)

// toolEvents 去掉模型调用摘要，只留工具步骤，给按条数断言工具记录的测试用。
func toolEvents(evs []RunEvent) []RunEvent {
	var out []RunEvent
	for _, ev := range evs {
		if ev.Kind != KindModelCall {
			out = append(out, ev)
		}
	}
	return out
}

func modelEvents(evs []RunEvent) []RunEvent {
	var out []RunEvent
	for _, ev := range evs {
		if ev.Kind == KindModelCall {
			out = append(out, ev)
		}
	}
	return out
}

func TestEveryModelCallIsLoggedWithoutContent(t *testing.T) {
	h := newHarness(t, echoSpec(),
		goodStep("s1", "13800138000"),
		llm.Inject(llm.FaultStreamTruncated),
		llm.Say("营收是 96 万"),
	)
	out, err := h.run()
	if err != nil || out.State != StateCompleted || out.ModelCalls != 3 {
		t.Fatalf("%+v %v", out, err)
	}
	got := modelEvents(h.log.Events())
	if len(got) != out.ModelCalls {
		t.Fatalf("模型调用 %d 次，记录 %d 条: %+v", out.ModelCalls, len(got), got)
	}
	if got[0].Status != "ok" || got[0].Summary != "请求工具 echo" {
		t.Fatalf("第一次 %+v", got[0])
	}
	if got[1].Status != "failed" || got[1].Code == "" || got[1].Summary != "模型调用失败" {
		t.Fatalf("第二次 %+v", got[1])
	}
	if got[2].Status != "ok" || got[2].Summary != "最终回答 8 字" {
		t.Fatalf("第三次 %+v", got[2])
	}
	for _, ev := range got {
		if strings.Contains(ev.Summary, "13800138000") || strings.Contains(ev.Summary, "96") || ev.ParamsHash != "" {
			t.Fatalf("模型调用摘要带了正文或参数: %+v", ev)
		}
	}
	steps := toolEvents(h.log.Events())
	var stepID string
	for _, ev := range steps {
		if ev.Kind == "step" {
			stepID = ev.StepID
		}
	}
	if stepID != "s1" {
		t.Fatalf("工具记录没有带上步骤号: %+v", steps)
	}
}
