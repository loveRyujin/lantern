// Package tools 是工具注册表和参数校验。
//
// 一次调用的处理顺序固定为：完整性（截断）→ JSON 解析 → Schema → Policy →
// 需要时生成确认单 → 执行。前一步失败，后面的步骤不会发生。
// 不补括号，不截取前缀，也不把残缺参数交给确认单。
//
// 错误码（产品已定，issue 正文里的 tool_args_truncated 等写法作废）：
//
//	tool_arguments_incomplete  输出被截断，参数不完整
//	tool_arguments_invalid     JSON 不合法，或不符合 Schema
//	tool_call_skipped          同一步里有别的调用不合法，本调用被取消
//
// run_sql、run_python、export_data、write_action、search_docs 用同一套
// [Tool] 接口注册。本包装它们的 Schema 和桩，不实现业务。
// run_sql 的执行由只读 SQL 工具接 [SpecRunSQL]，确认单的落库由审批状态机接
// [approval.Store]。
package tools
