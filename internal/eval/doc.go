// Package eval 实现 lantern-eval 的用例加载、mock 计分和报告。
//
// 用例来自目录下的 YAML（.yaml / .yml）和 JSONL。每条用例包含 id、category、
// input、expected_behavior（别名 expected）和判定规则。判定规则的规范形状是
// judging_criteria.deterministic，另可用 rule 作为别名。确定性规则写成
// “键:期望值”；以 _not 结尾的键是禁止形式，例如 tool_call_not:export_data。
//
// 规则键必须出现在规则清单里。清单默认是用例目录下的 rules.yaml，路径可用
// Options.RulesPath 或命令行 --rules 覆盖。#17 合并前清单格式以本包解析器为准：
// rules 或 keys 字段，值是字符串列表，或键到说明的映射。清单外的规则判该用例
// 失败，进程以非零退出，不静默跳过。
//
// mock 模式不访问网络。观测值来自 input.mock；计数类缺省为 0，布尔类缺省为
// false。LLM 裁判只在确定性规则全部通过且 llm_judge.required 为 true 时调用，
// 默认模型是 gemini-4-argon。
package eval
