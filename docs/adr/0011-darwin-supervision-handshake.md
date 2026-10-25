# ADR 0011：Darwin 单故障互监与启动交接

## 状态与问题

候选已取得 18 项原生通过基线（#54/PR55）；本 ADR 固定监督合同，最终交付以独立变异和合并/main 验收为准。#52 已证明固定隔离与资源实验可运行，也实际发现 guardian 死亡后任务仍存活。生产 Darwin Runner/SQL worker 仍保持不可用；本轮只收敛这一监督缺口的故障模型与固定实验，不接入模型任意代码。

## 故障模型

一次仅一个可信进程意外退出或被 SIGKILL，其余观察者存活且可调度。应用和 guardian 任一方死亡都应由存活方终止任务、确认退出并清理。双杀、全部观察者 SIGSTOP、内核/整机故障与同用户主动篡改不在已承诺单故障范围内。异步 pipe EOF/kqueue 不是内核原子 Pdeathsig，不承诺对方死亡瞬间之后绝无一次 exec。

主候选复用应用作为第二监管者，不先增加第二个额外 guardian：guardian 是任务的直接父进程；应用保存任务身份，guardian 死亡后负责终止并观察孤儿实际消失。只有直接父进程才能 Wait，不能把应用观察 orphan absence 写成应用 Wait 了孙进程。

## 先登记和 armed，再 GO

仅在 guardian 启动后把 PID 转发给应用仍有漏洞：guardian 可能在 spawn 任务后、转发前死亡。候选由可信 launcher 直接向应用登记 nonce/PID/birth/允许镜像，在应用登记且 guardian 监测 armed 前阻塞；应用直接授权 GO，之后同一 PID 才 exec 固定 payload。

```mermaid
sequenceDiagram
    participant App as 应用
    participant Guard as guardian
    participant Launch as 可信 launcher
    participant Task as 固定 payload
    App->>Guard: 启动，专有生命管道
    Guard->>Launch: spawn，登记和 GO 的专用 FD
    Launch->>App: PREPARED，nonce 与进程身份
    Guard->>App: 直接子任务与监测已 armed
    App->>Launch: 已登记且 armed，授权 GO
    Launch->>Task: 同 PID exec，控制 FD 关闭
    Note over App,Guard: 任一单故障由仍存活且已知身份的一方收回任务
```

登记 writer 的副本在 guardian spawn 后关闭，仅 launcher 保留；GO writer 仅应用持有；应用生命 writer 也不得被 launcher/payload 继承。执行前关闭或设置 CLOEXEC，防止不相关副本遮蔽 EOF。每条消息有固定版本、长度预算、job nonce 和时序约束，协议失败或有界超时不得 GO。

PID/birth/镜像复核不是 pidfd 原子句柄。允许的镜像包含已验证可信启动链，任务 setsid/setpgid 不改变登记 PID。并发自然退出、错误、取消与正常 helper drain 必须有明确状态机，不能在成功回收前解除监督，也不能把双方正常退出误判成需要误杀的故障。

## 固定实验与出口

独立 `supervision` 子命令与 `lantern.darwin-supervision.v1` 报告，不改变 #52 的八阶段、65 检查和原 guardian 负面观测。固定 18 项：四启动窗分别注入应用/guardian 故障（8）；稳态故障（2）；setsid 后故障（2）；setpgid 后故障（2）；正常、错误、取消、退出与取消并发（4）。

首窗明确为 guardian 已存在、尚未 spawn 任务 launcher；其余为 spawn 后应用未接受登记、已登记尚未 armed、armed 尚未 GO。通过事件和 ack 精确暂停注入，不用 sleep 猜测。每 case 最长 8 秒、整轮 4 分钟、故障至终止/身份消失最长 2 秒，worker 初始 Wall 不超过 5 秒；这些是待实际验证的实验预算。

成功必须同时满足阶段、真实身份/进入或未进入证据、直接 Wait 与孤儿消失、所有自有进程终结、目录清理，且 harness_rescue=false。harness 仅在失败后做明确标记的救援，不能把救援变成候选机制能力。基线完整后，独立移除关键监督与 GO 门控，必须出现对应真实目标失败，不能用编译、前检或外层超时假红。

若候选仍不可行，可据原生证据形成 negative 研究结论；生产监督器依赖仍阻塞。最终提交的专项、全量、平台/原生 CI、独立变异、review 正文、合并/main 与 lantern-notes 同轮完成才收口，W4 不随本轮研究关闭。

## 原生基线与信号安全的收敛

首版 c7f996b 的真实 macOS 15.7.9/arm64/Python3.12.10 运行中，15/18 项通过，三个未 GO 的启动窗口失败。原始事件在故障后很快已有直接子进程 Wait 与目录清理，最终全部进程也消失；身份读取恰逢退出被统一写成 birth mismatch，永久 receipt.Error 又使 driver 等满期限。首次全部身份消失的时间当时未独立记录，不能从 complete 事件反推出精确退出时延。

修复严格区分“成功读取但 birth 不同”与“身份读取不可用/退出中”：前者永久失败、绝不发信号；后者记录 signal_skipped，仅由真实 own Wait 加目标消失，或应用已登记身份加最终消失来收敛。SignalSkipReason/SignalResolvedBy 保留原始事实，不清除其它错误。fault_to_absent 在第一次观察到全部原身份消失时锁定，与最终 receipt 是否有效分离，避免把证据失败等待时间伪成终止延迟。

1657039 的后续原生基线完整通过 18 项/144 检查；独立 QA 对照原始 JSONL 与报告事件、启动无 GO/无进入、exec 前后同 PID、FD/stdin/no-fork、实际 setsid/setpgid、直接 Wait 与孤儿消失及 no-rescue。单次 CI 最大故障至消失约 20.36ms、case 约 163.65ms，只是这次运行观测，不是跨机器性能保证。

事件目录必须排他新建。已有 out/events 时在 actor 启动、读取旧 PID 或注入之前拒绝；不覆盖旧报告/事件、不删除旧数据，结构化失败由 stdout 返回。不能先使用陈旧目标再凭最终 validator 报错补救。

独立变异包括应用 peer-loss 动作、guardian peer-loss 动作及无 GO 的可信 launcher 对照。第三项必须区分真实 helper 在未授权执行后正常退出 0 与独立 oracle 拒绝该行为退出 1，不伪造被测程序错误码，也不把单 helper 对照冒成完整 18 项报告。完整原生执行、原始事件/hash/恢复记录与最终 CI 是交付门槛。
