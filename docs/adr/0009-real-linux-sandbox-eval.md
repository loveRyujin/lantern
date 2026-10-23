# ADR 0009：独立版本化的真实 Linux 沙箱评测

## 状态与目标

实现与验收中（#44，2026-10-23）。#38/#40–#43 已分别完成 SQL worker、平台决策、真实 Runner、数据链与生产 Python 工具；本轮形成 W3 的可复现评测出口，不代替 W4 macOS 或全项目云模型 live 评测。

## 保留历史，另建真实契约

冻结的 eval/v1 保留 Mock 基线：Linux 选择时加载 237、评分并通过 217，另 20 为平台过滤；all 模式及旧 scoring digest 不变。新增 `sandbox-real`，使用 eval/sandbox-real/v1 独立清单，不能污染旧递归加载范围。真实执行完全忽略 input.mock，不从历史预设结果补观测。

原 SB-LINUX-01–20 每项保留来源 ID、历史指纹与意图；新断言按已批准的 Linux 单进程/单线程 profile 版本化。尤其不能把实际 EPERM 改写为历史 killed、output_limit、permission_denied；符号链接逃逸应记录实际 attempt>0 与成功逃逸=0，而非声称未发生尝试。

补证为 SB-LINUX-21 累计 CPU、22 父应用死亡、23 强制隔离不可用、24 生产 Session 出图、25 运行中取消。20 原意图与 5 补证分别统计；完整性要求精确必需 ID 集、全部评分、零 skip/missing/unknown。缺项、重复、未知、缺正控或整体环境不支持均失败关闭，不允许筛掉失败项仍宣告完整验收。

## 观测来自实际操作

每项保存真实 Run 结果、errno/稳定错误码、有效预算、RSS/CPU/Wall/MaxSampleGap、PID 回收、临时目录清理、固定 payload 指纹、规则及来源版本。预期执行失败与评测失败是不同概念：fork/setsid 被内核拒绝而脚本正常返回，可为安全通过；不能为满足旧码伪造进程被杀。

网络使用受控 TCP IPv4/IPv6、UDP DNS-like 与 Unix listener，宿主实际连接/接收作正控；socket 创建成功即违反默认全禁网，不依赖公网故障制造假绿。raw INET 可能在宿主已因无 CAP_NET_RAW 返回 EPERM，应标 ambient/归因限制，不能单独证明 seccomp；补可行的 NETLINK RAW 与普通 TCP/UDP 强正控。必需 listener 或探针前提失败应失败，不跳过。

文件使用同用户可读写的合成 sentinel 验证隔离，而非利用本来无权读取的宿主凭据假绿。实际敏感路径只 open/close 记录结果，不输出内容，不改真实 SSH/系统配置。合法 out/tmp 写入应通过；只读输入、越界、symlink 逃逸分别断言且校验 sentinel 未改变。

进程使用有限 fork/thread/setsid 探针，若防护被移除，成功创建的子进程立即退出并 Wait，绝不真正运行无界炸弹。setuid(currentUID) 是宿主可成功正控；setuid0 的基础权限拒绝另记。ptrace 仅使用不存在目标/无副作用参数，不 TRACEME，不附加真实应用。

## 资源与生命周期

CPU 用低累计时间预算和较高 Wall 的忙循环，须在 Wall 前终止；Wall 睡眠、RSS 触页分配和大虚拟地址正例分别证明口径。采样超限终止保留真实峰值与间隔，不声称瞬时 RSS quota 或固定超调字节。

父死补证在可信独立应用 helper 中运行真实 Runner，看到 Python ready 后仅终止该 helper，验证 Python/guardian 回收和目录清理；不能只凭禁止 fork 就声称父死正确。取消、异常与成功也验证清理及后续可用。

强制不可用补证需在受控可信 helper 中真实剥夺隔离能力，验证 sandbox_unavailable 与用户 payload 未进入，而非配置一个假观测。缺 runtime 和 Schema/Policy 前置拒绝可预期无 Python 启动，但必须基于先通过的整体正常运行前检，不可借其掩盖整个环境失效。

## 运行时、报告与执行命令

可信操作员显式 `lantern-eval prepare-runtime --root <目录> --out <新配置.json>` 准备依赖；`run --mode sandbox-real --runtime-config <配置>` 只读验证并使用租约，不安装、不修复、不降级。输出配置有界、完整发布且不覆盖旧文件。

报告环境明确 sandbox-real/固定代码，无云 LLM、无 embedding；良性补证由固定脚本驱动真实 Agent 的工具选择，SQL/Python 观察值来自实际执行，不是 input.mock。记录真实 kernel、Landlock ABI、UID/capabilities、Python、实现/profile、提交/dirty 状态与用例版本。不填 Grok 或虚构维度。外泄/审批等无样本全项目门槛标 not-applicable，不伪造零观测使其全绿；本模式只对完整 sandbox 契约评分。

Runner 返回归一化配置预算，工具观察值及评测沿用它；安装失败时仍须明确“已配置”不等于“已生效”。父应用被杀后无 Runner 回包，标 not-returned 并保存实际应用退出状态，不创造稳定错误码。父死救援清理使用 pidfd，防止原 PID 回收后误杀复用 PID 的其他进程；实际 Wait 回收且再次检查目录消失，不以 Close 返回 nil 代替清理证据。

OOM 与流输出超限除了最终错误码，还必须在固定五秒休眠结束前及时终止，避免 Wait 后补分类冒充在线限制。输出负载保持有界，防护移除实验也不会无限灌内存。文件探针实际尝试 6 MiB，记录 EFBIG、5 MiB 完整产物和宿主可写正控。良性 Agent 补证先真实生成缺 IDAT 的 PNG 并证明收集拒绝，再根据工具错误反馈修正为可解码图表；保存实际 PNG 哈希、字节与尺寸。

JSON 和 Markdown 都保留证据与失败原因，CI 上传报告 artifact。确定性评分指纹与单次动态证据需区分，不能把 PID/采样时长差异当行为退化，也不能宣称 scoring digest 鉴证了所有运行时证据字节。

## 最终出口

独立 QA 验证实际防护移除后出现真实访问/创建或预算未终止，并在行为断言失败；破坏 fixture/manifest/观测同样非零。保留 Mock Linux/all 指纹回归。最终 SHA 全量检查、真实 CI、审查正文、三角色验收、合并/main、artifact 与 lantern-notes 完成后，逐条核对 W3 历史出口再关闭 milestone。
