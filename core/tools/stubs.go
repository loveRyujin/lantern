package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// 五个工具的 Schema。执行由各自的实现注册；这里的桩只证明接口能登记。
// run_sql 的参数是 sql（必填）和 limit（可选整数）。业务执行不在本包。

var (
	// SpecSearchDocs 是检索工具的注册说明。
	// 执行在 rag.SearchTool。k 的 maximum 必须和 rag.MaxSearchK 一致。
	// 引用格式 [[cite:chunk_id]] 由 Agent 循环校验，不写进 Schema。
	SpecSearchDocs = Spec{
		Name:        "search_docs",
		Description: "在当前工作区的知识库中检索口径文档。只查打开的那一个 kb.db。query 必填，k 是可选的 top-k。最高分低于阈值时返回未找到依据，不附带低分片段。引用片段时写成 [[cite:chunk_id]]，chunk_id 必须来自本次运行已经返回的检索结果。",
		Parameters: json.RawMessage(`{
			"type": "object",
			"additionalProperties": false,
			"required": ["query"],
			"properties": {
				"query": {"type": "string", "minLength": 1, "maxLength": 8192},
				"k": {"type": "integer", "minimum": 1, "maximum": 20}
			}
		}`),
		MaxAttempts: 1,
	}
	// SpecRunSQL 是只读 SQL 工具的注册说明。#34 按这个 Schema 注册真正的执行。
	// limit 不传时，执行层默认返回 200 行。这里的 maximum 是 1000，再大就在执行前被 Schema 拒绝。
	// 结果超过生效的行数时截断，并在结果里标 truncated。
	SpecRunSQL = Spec{
		Name:        "run_sql",
		Description: "在当前工作区的分析库上执行只读查询",
		Parameters: json.RawMessage(`{
			"type": "object",
			"additionalProperties": false,
			"required": ["sql"],
			"properties": {
				"sql": {"type": "string", "minLength": 1, "maxLength": 65536},
				"limit": {"type": "integer", "minimum": 1, "maximum": 1000}
			}
		}`),
		Timeout:     0,
		MaxAttempts: 1,
	}
	// SpecRunPython 是沙箱代码工具的注册说明。
	// limits / extra_read_only / network 交给 Policy：超默认资源或额外只读目录要确认，
	// 网络（含回环）直接拒绝。requires_approval 和 approved 可以出现，但不改变判定。
	// 运行时路径和任意输入仍然不是合法参数。
	SpecRunPython = Spec{
		Name:        "run_python",
		Description: "在禁网沙箱中执行 Python 分析；可用 sql_call_id 引用同一运行中成功的 run_sql 调用，用 load_table() 读取 (columns, rows, metadata)。图表写入 OUTPUT_DIR，返回可取回产物 ID。limits 中提高 cpu_ms、memory_bytes、wall_ms 或 nproc（不超过硬上限），或给出 extra_read_only，会进入本人确认。network 只要不是 false（含 loopback、127.0.0.1、::1）就直接拒绝，不能通过确认放开。requires_approval 和 approved 会被忽略。",
		Parameters: json.RawMessage(`{
			"type": "object",
			"additionalProperties": false,
			"required": ["code"],
			"properties": {
				"code": {"type": "string", "minLength": 1, "maxLength": 1048000},
				"sql_call_id": {"type": "string", "minLength": 1, "maxLength": 256},
				"limits": {
					"type": "object",
					"additionalProperties": false,
					"properties": {
						"cpu_ms": {"type": "integer", "minimum": 0},
						"memory_bytes": {"type": "integer", "minimum": 0},
						"wall_ms": {"type": "integer", "minimum": 0},
						"nproc": {"type": "integer", "minimum": 0}
					}
				},
				"extra_read_only": {
					"type": "array",
					"items": {"type": "string", "minLength": 1, "maxLength": 4096}
				},
				"network": {
					"anyOf": [
						{"type": "boolean"},
						{"type": "string", "minLength": 1, "maxLength": 256}
					]
				},
				"requires_approval": {},
				"approved": {}
			}
		}`),
		MaxAttempts: 1,
	}
	// SpecExportData 是敏感导出工具的注册说明。确认由 Policy 决定，不写在 Schema 里。
	SpecExportData = Spec{
		Name:        "export_data",
		Description: "导出数据",
		Parameters: json.RawMessage(`{
			"type": "object",
			"additionalProperties": false,
			"required": ["object"],
			"properties": {
				"object": {"type": "string", "minLength": 1},
				"fields": {"type": "array", "items": {"type": "string"}}
			}
		}`),
	}
	// SpecWriteAction 是写操作工具的注册说明。确认由 Policy 决定。
	SpecWriteAction = Spec{
		Name:        "write_action",
		Description: "执行写操作",
		Parameters: json.RawMessage(`{
			"type": "object",
			"additionalProperties": false,
			"required": ["action"],
			"properties": {
				"action": {"type": "string", "minLength": 1},
				"product_ids": {"type": "array", "items": {"type": "string"}}
			}
		}`),
	}
)

// Stub 是一个能注册、但执行时明确返回未实现的工具。
type Stub struct {
	spec Spec
}

// NewStub 用 spec 做一个桩。
func NewStub(spec Spec) Stub {
	return Stub{spec: spec}
}

// Spec 实现 Tool。
func (s Stub) Spec() Spec { return s.spec }

// Execute 不执行业务。
func (s Stub) Execute(ctx context.Context, call Call) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return Result{}, fmt.Errorf("tools: 工具 %s 还没有实现", call.Name)
}

// RegisterBuiltins 登记五个桩。同名工具已经存在时返回错误。
// 真正的 search_docs 用 rag.SearchTool 和 [SpecSearchDocs] 登记，不要和桩一起注册。
// 真正的 run_sql 不要和桩一起注册，自己用 [SpecRunSQL] 登记实现。
func RegisterBuiltins(r *Registry) error {
	for _, spec := range []Spec{SpecSearchDocs, SpecRunSQL, SpecRunPython, SpecExportData, SpecWriteAction} {
		if err := r.Register(NewStub(spec)); err != nil {
			return err
		}
	}
	return nil
}
