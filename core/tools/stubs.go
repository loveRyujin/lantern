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
	SpecSearchDocs = Spec{
		Name:        "search_docs",
		Description: "在当前工作区检索口径文档",
		Parameters: json.RawMessage(`{
			"type": "object",
			"additionalProperties": false,
			"required": ["query"],
			"properties": {
				"query": {"type": "string", "minLength": 1},
				"k": {"type": "integer", "minimum": 1}
			}
		}`),
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
	SpecRunPython = Spec{
		Name:        "run_python",
		Description: "在沙箱里执行 Python",
		Parameters: json.RawMessage(`{
			"type": "object",
			"additionalProperties": false,
			"required": ["code"],
			"properties": {
				"code": {"type": "string", "minLength": 1}
			}
		}`),
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
// 真正的 run_sql 不要和桩一起注册，自己用 [SpecRunSQL] 登记实现。
func RegisterBuiltins(r *Registry) error {
	for _, spec := range []Spec{SpecSearchDocs, SpecRunSQL, SpecRunPython, SpecExportData, SpecWriteAction} {
		if err := r.Register(NewStub(spec)); err != nil {
			return err
		}
	}
	return nil
}
