package eval

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// DefaultJudgeModel 是 LLM 裁判的默认模型。
const DefaultJudgeModel = "gemini-4-argon"

// ErrJudgeUnavailable 表示 live 裁判还没有接入，调用不会访问网络。
var ErrJudgeUnavailable = errors.New("live 裁判未接入")

// Request 是一次裁判请求。确定性规则失败时不会构造请求。
type Request struct {
	CaseID   string
	Category string
	Input    any
	Expected string
	Rubric   string
	Rules    []string
}

// Verdict 是裁判结论。Mock 模式的结论只由用例里的脚本决定，两次调用一致。
type Verdict struct {
	Pass   bool
	Score  float64
	Reason string
	Model  string
}

// Judge 给引用忠实度一类语义题打分。mock 实现不访问网络。
type Judge interface {
	Model() string
	Judge(ctx context.Context, req Request) (Verdict, error)
}

// JudgeSpec 是用例里的 llm_judge 配置。
type JudgeSpec struct {
	Required bool
	Model    string
	Rubric   string
}

// NewJudge 按模式返回裁判。model 为空时使用 DefaultJudgeModel。
func NewJudge(mode, model string) (Judge, error) {
	if model == "" {
		model = DefaultJudgeModel
	}
	switch mode {
	case ModeMock, "":
		return mockJudge{model: model}, nil
	case ModeLive:
		return liveJudge{model: model}, nil
	default:
		return nil, fmt.Errorf("eval: mode 必须是 mock 或 live")
	}
}

type mockJudge struct {
	model string
}

func (j mockJudge) Model() string { return j.model }

func (j mockJudge) Judge(ctx context.Context, req Request) (Verdict, error) {
	if err := ctx.Err(); err != nil {
		return Verdict{}, err
	}
	if score, ok := scriptedJudge(req.Input); ok {
		return Verdict{
			Pass:   score >= 1,
			Score:  score,
			Reason: "mock: 按 input.mock.judge_pass 裁决",
			Model:  j.model,
		}, nil
	}
	return Verdict{
		Pass:   true,
		Score:  1,
		Reason: "mock: 默认通过",
		Model:  j.model,
	}, nil
}

type liveJudge struct {
	model string
}

func (j liveJudge) Model() string { return j.model }

func (j liveJudge) Judge(ctx context.Context, _ Request) (Verdict, error) {
	if err := ctx.Err(); err != nil {
		return Verdict{}, err
	}
	return Verdict{Model: j.model}, fmt.Errorf("eval: %w（模型 %s）", ErrJudgeUnavailable, j.model)
}

func scriptedJudge(input any) (float64, bool) {
	mock, ok := mockMap(input)
	if !ok {
		return 0, false
	}
	if raw, ok := mock["judge_score"]; ok {
		score, ok := asFloat(raw)
		return score, ok
	}
	if raw, ok := mock["judge_pass"]; ok {
		switch v := raw.(type) {
		case bool:
			if v {
				return 1, true
			}
			return 0, true
		case string:
			switch strings.TrimSpace(v) {
			case "true":
				return 1, true
			case "false":
				return 0, true
			}
		}
	}
	return 0, false
}
