# ADR 0012：将 Darwin 单故障监督收敛为内部 API

状态：实现与原生验收中；关联 Issue #56，前置 ADR 0011 / Issue #54。

## 问题与范围

#54 证明了固定 Python 实验的双观察者握手，但命令中的实验宿主不是业务调用者。后续 Darwin Runner 需要可嵌入的监督核心：调用 `Run` 的进程本身持有生命周期与 GO 写端，死亡时由内核关闭，而不是在库内另起一个假宿主。

本轮仅实现 `internal/supervisor` 的 sealed GoLifecycle。它执行仓库固定 helper，不接受任意 executable、argv、环境、ExtraFiles 或用户代码，不接入 SQL/Python Runner，也不提供资源配额。旧 #52/#54 Python、Mach-O、SBPL 实验保留原实现与历史 CI；它们不声称通过本 API。没有 LegacyPython/PreparedRuntime 占位。新 core 是后续生产监督实现的唯一演进路径。

## 接口与状态

`New(Config).Run(ctx, Request) (Result, error)` 为阻塞接口；每次调用创建独立 nonce、工作目录和 IPC，同一个不可变 Supervisor 可并发调用。没有通用 Start/Wait/Close handle 框架。取消必须等待监督和清理结束才返回，返回后的取消无效。

`Completed` 与 nil error 仅说明监督与回收完成。`Worker.ExitCode == 1` 仍表示 worker 失败，调用者必须单独检查，不能将 nil error 当业务成功。取消、协议失败、观察者丢失和清理失败保留监督错误。宿主被 SIGKILL 时不可能返回 Result；验收输出 `returned=false`，另外保存实际 guardian receipt，不伪造宿主成功结果。

## 机制与取舍

宿主持有唯一 life/GO 写端。Guardian 与 Launcher 经私有 IPC 交换 nonce、出生时间、固定 image 和顺序帧。Launcher 的直接身份注册与 Guardian armed 确认先于 GO，同 PID exec 固定 Go worker。提前 bootstrap 仅帮助识别待回收 launcher，不构成 Registered 或 GO 授权。

Darwin Go 的普通进程创建不能假设自动关闭所有未标 CLOEXEC 的宿主 FD，因此使用 `posix_spawn` 的 `POSIX_SPAWN_CLOEXEC_DEFAULT` 与显式白名单，Launcher 使用 SETEXEC 保留注册 PID。白名单先复制到私有高位 FD，避免源与目标编号交换时覆盖。库不修改宿主环境、rlimit、cwd 或非自身 FD 的 flags，不在宿主中 exec/exit。

Go runtime 可以创建自己的 kqueue 等 FD。验收按 dev/inode/mode 检查凭据及控制端点是否被继承，不要求 Go worker 只有 0–2；旧 Python 实验的历史约束不变。EvidenceRoot 是保留诊断的输出，不读取日志中的 PID 作为 core 控制指令。

信号前检查 PID 与出生身份，真实不同身份禁止发送；这仍不是 pidfd 原子绑定。僵尸不能算已消失。单故障模型要求另一观察者存活且可调度，不覆盖双杀、全体 SIGSTOP、重启或内核级原子父死保证。

## 验证计划与证据边界

新的 `supervision-api --out NEW_DIR` 直接使用新 API：4 个启动窗口 × 宿主/guardian，steady、setsid、setpgid × 两种故障，以及 normal/error/cancel/exit-cancel-race，共 18 场景。一次性输出目录防止旧 receipt 混入。真实宿主 fixture 在自己的 PID 调用 Run；外层验收进程仅负责确定性注入 SIGKILL、独立观测与记录救援。

独立 QA 另外验证 normal/error/cancel 后恢复、同 Supervisor 并发取消隔离、返回后取消、宿主 invariants、非 CLOEXEC 凭据 FD 和环境隔离。CI 必须检查精确场景集合、实际原生测试 receipt 与最终 source SHA，防止空匹配测试被当作通过。交叉编译和 portable 测试不替代 macOS 原生结果。最终实测结果、失败原因和面试问答在同轮 lantern-notes 更新。

## 面试重点

- 为什么宿主不能是库创建的替身进程？替身仍存活时真实调用者死亡无法直接关闭唯一生命写端。
- 为什么 nil error 不代表 worker 成功？监督负责生命周期，worker 的退出码负责工作结果，两者独立。
- 为什么 ExtraFiles 不足？宿主可能有未设置 CLOEXEC 的第三方 FD，需要创建时默认关闭与显式继承。
- 为什么不通过临时修改全部 FD flags 修复？这会污染并发宿主，创建前后存在竞争。
- 为什么保留旧实验而不立即统一 Python backend？Python 的运行时读取闭包、Seatbelt 与签名是另一组责任；本轮先验证可嵌入生命周期核心，旧历史证据不冒充新路径证据。

## 原生验收发现的 SETEXEC 差异

首轮候选 ee3df10 的 8 个启动故障场景通过，其余 10 个场景在 worker 入场前失败。第二轮 0d4c890 加入原始错误记录，实际得到 `bad file descriptor`，不是生命周期超时。Apple `kern_exec.c` 的 SETEXEC 路径先进行 `CLONEPROC_EXEC`；`kern_descrip.c` 的 `fdt_fork(in_exec=true)` 会跳过 CLOEXEC 源，随后文件动作引用这些源会得到 EBADF。普通 spawn 不走这个源过滤，解释了 guardian 与 launcher 能启动而同 PID exec 失败。

修复只在已隔离 launcher 的 SETEXEC 分支为私有高位副本使用 F_DUPFD；宿主普通 spawn 仍用 F_DUPFD_CLOEXEC，既有 FD flags 不变。CLOEXEC_DEFAULT 始终保留，内核只给 dup2 目标标记 UF_INHERIT，临时源在最终 exec 时关闭。该修复必须再次接受真实 worker FD/control 与宿主凭据正控检查，源码推导不能替代原生验收。

一手依据：[kern_exec.c](https://raw.githubusercontent.com/apple-oss-distributions/xnu/xnu-11215.81.4/bsd/kern/kern_exec.c)、[kern_descrip.c](https://raw.githubusercontent.com/apple-oss-distributions/xnu/xnu-11215.81.4/bsd/kern/kern_descrip.c)。
