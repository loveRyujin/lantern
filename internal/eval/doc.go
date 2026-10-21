// Package eval 实现 lantern-eval 的用例加载、mock 计分和报告。
//
// 用例来自目录下的 YAML（.yaml / .yml）和 JSONL。每条用例包含 id、category、
// input、expected_behavior（别名 expected）和判定规则。判定规则的规范形状是
// judging_criteria.deterministic，另可用 rule 作为别名。确定性规则写成
// “键:期望值”。
//
// 以 _not 结尾的规则是禁止形式，例如 tool_call_not:export_data。input.mock 没有
// 同名观测时，回退读取去掉 _not 的基键（tool_call_not 读取 mock.tool_call）。
// 基键和完整键都没有观测时按缺观测失败，不把“没观测”当成“禁止值没有出现”。
//
// 规则键必须出现在规则清单里。清单默认是用例目录下的 rules.yaml，路径可用
// Options.RulesPath 或命令行 --rules 覆盖。清单外的规则判该用例失败，进程以非零
// 退出，不静默跳过。计数类键是名字以 _count 结尾，或确定性参数类型为 integer /
// integer_or_symbol 的规则。
//
// 硬门槛在独立的 gates.yaml，默认是用例目录下的 gates.yaml，路径可用
// Options.GatesPath 或 --gates 覆盖。门槛引用清单里没有的键，或计数类键既没有
// 归入门槛也没有列在 ungated，启动即报错，不写报告。规则清单写了 version 时必须
// 和门槛文件一致；清单没有 version 时不因此失败。
//
// 用例引用了门槛里的键且观测值不等于期望值，记该门槛一次失败，阈值是 0。
// side_effect 里带 categories 的键只统计类别等于该前缀、或以前缀加 "." 开头的用例。
// 样本只统计确实有观测的值。某道已加载门槛的样本数为 0 时，报告写「无样本」和
// 「未验证」，Passed 为 false，进程非零退出。没有加载门槛文件、且没有用例时，
// 仍写出空报告并以 0 退出。
//
// mock 模式不访问网络，也不把计数缺省成 0。没有 input.mock 的用例状态是 skipped，
// 不进通过率分子和分母。input.mock 存在但缺规则要的键时，状态是 missing_observation，
// 计入非零退出。确定性规则失败或出现缺观测时不调用裁判。mock 裁判默认不通过，
// 只有 input.mock 里有 judge_pass 或 judge_score 才按脚本给结论。
//
// --mode live 在接入前直接返回错误，错误文本包含「live 未接入」，不写报告。
// 报告里的模型、embedder 和沙箱实现在 mock 运行中写 mock。embedder_dim、
// vector_store、chunk_strategy 是约定值，不是这次运行测到的实现。
package eval
