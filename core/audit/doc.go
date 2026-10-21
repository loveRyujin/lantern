// Package audit 是只追加的审计日志。
//
// 应用层只提供追加。runs.db 里的 audit 表用触发器拒绝 UPDATE 和 DELETE。
// 写入失败步骤的原始参数不属于审计内容。
package audit
