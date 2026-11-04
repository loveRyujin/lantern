package policy

import (
	"context"
	"encoding/json"
)

// Request 是一次已经通过 Schema 的工具调用。
// Args 是调用方给的副本。策略可以读取它；就地修改不会影响执行。
// Static 不读 Args。Gate 只在 run_python 上读取资源、额外只读目录和网络。
type Request struct {
	Tool string
	Args json.RawMessage
}

// Decision 是要不要确认。Deny 为 true 时不执行，也不弹确认。
type Decision struct {
	NeedsConfirm  bool
	Deny          bool
	Reason        string
	Sensitive     []string
	EstimatedRows int64
	Risk          string
}

// Policy 在参数合法之后决定确认。它不解析模型自由文本。
type Policy interface {
	Decide(ctx context.Context, req Request) (Decision, error)
}

// Static 按工具名决定。名单里的工具必须确认，其余直接放行。
// 名单是注册信息，不是模型参数。
type Static struct {
	Confirm map[string]struct{}
}

// Default 要求 export_data 和 write_action 按工具名确认，
// 并对 run_python 按参数判定沙箱超权限。
func Default() Gate {
	return Gate{Names: Static{Confirm: map[string]struct{}{
		"export_data":  {},
		"write_action": {},
	}}}
}

// Decide 实现 Policy。Args 故意不参与判断。
func (s Static) Decide(ctx context.Context, req Request) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	if _, ok := s.Confirm[req.Tool]; !ok {
		return Decision{}, nil
	}
	return Decision{
		NeedsConfirm: true,
		Reason:       "该动作需要本人确认",
		Risk:         "将执行 " + req.Tool,
	}, nil
}
