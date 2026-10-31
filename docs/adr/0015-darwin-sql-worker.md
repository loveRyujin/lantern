# ADR 0015：Darwin SQL worker 的隔离进程与内存上限

状态：实现对应 Issue #65。macOS CI 证据以该 PR 的 `macos-sql-worker` 作业为准，不能用 Linux 交叉编译代替。不关闭 W4 里程碑，不开始 W5。

## 问题

Linux SQL worker（#38/#39/#63/#64）已经用第二次 exec、`RLIMIT_AS=1GiB`、只读合同、`close_range` 和进程组回收把模型 SQL 挡在父进程外面。Darwin 监督（#52–#61）合入之后，`NewTool` 仍走 `process_other.go`，查询返回 `sql_worker_unavailable`。两平台问数因此在 macOS 上失败。

## 决策

Darwin 使用与 Linux 同构的第二次 exec：`exe --lantern-sql-limit-v1` 再 exec `exe --lantern-sql-worker-v1`。shim 只把 core dump 上限设为 0。只读合同不变：`mode=ro`、`query_only`、拒绝 ATTACH、路径白名单、查询前后文件身份和哈希。协议仍是 ready/done，`memory_limit_bytes` 必须等于 `ProcessMemoryBytes`（1,073,741,824）。在 Darwin 上这个字段表示双方都在执行的物理足迹停止线，不是已经装好的 `RLIMIT_AS`。环境整表替换为 `GOMAXPROCS`、`GOMEMLIMIT` 和 `TMPDIR`，不继承 API Key。取消时杀进程组并 Wait。

内存上限不用下面三个看起来像硬限制的接口：

| 接口 | 为什么不拿它当停止线 |
| --- | --- |
| `setrlimit(RLIMIT_AS)` | Darwin 上 `RLIMIT_AS` 与 `RLIMIT_RSS` 都是 5。调用返回 `EINVAL`。手册写明该资源不被强制，mmap 分配可以越过写进去的数字 |
| `memorystatus_control` | XNU 在分派命令之前要求 root 或 `com.apple.private.memorystatus`。无 entitlement 的进程得到 `EPERM`，包括读取属性。GitHub macOS runner 不是这份 entitlement 的持有者 |
| `task_set_phys_footprint_limit` | jetsam 协调接口。普通进程上的实测是错误返回，随后的分配仍然成功。不能把返回值当成缺页配额 |

改用 `proc_pid_rusage(pid, RUSAGE_INFO_V2)` 的 `ri_phys_footprint`。它是 Activity Monitor 的 Memory 列，单位是字节，包含压缩和换出的匿名页，不包含未触页的地址预留。阈值仍是 1 GiB，不把这个数放大。

| | Linux | Darwin |
| --- | --- | --- |
| 内核机制 | 第二次 exec 之前 `RLIMIT_AS` 软硬都是 1 GiB | 没有对应的、普通进程能核实的内核配额 |
| 计量对象 | 整个虚拟地址空间，含 Go 未触页预留 | `ri_phys_footprint`。未触页预留不计入 |
| 谁执行停止 | 内核让分配失败；进程通常还在 | worker 自己约每 2ms 读足迹，大于 1 GiB 就写运行时 OOM 文本并退出。父进程看见 worker 映像后以同样间隔采样，大于 1 GiB 就杀进程组 |
| 超调 | 无采样窗口 | 大约一个采样间隔，不是瞬时配额 |
| 看不见内存时 | 不适用 | 发 ready 前读不到自己的非零足迹则 125。已经看见 worker 后连续约 50 次读失败，杀进程并返回 `sql_worker_unavailable` |
| 父进程死亡 | `Pdeathsig=SIGKILL`，绑定创建线程 | 没有 `Pdeathsig`。worker 在第二次 exec 之后用 kqueue `NOTE_EXIT` 监视父进程，再杀自己的进程组。shim 到 exec 之间有一段窗口 |
| `wait4` 的 `ru_maxrss` | 千字节，父进程乘 1024 | 字节，而且是驻留峰值，不是物理足迹。不能乘 1024，也不能代替停止线 |
| fd≥3 | `close_range`（#64） | 本轮不实现 |

`GOMEMLIMIT=64MiB` 仍只降低 Go 堆压力，不是硬上限。kqueue 的 `NOTE_EXIT_MEMORY` 若出现，也归入超限；不依赖 jetsam 一定会发出这个事件。

失败关闭：足迹读不出来，或 worker 不确认 1 GiB 这个数，父进程返回 `sql_worker_unavailable`。不改用 `RLIMIT_AS`，不把读失败当成“没有上限也可以跑”，不在父进程里执行 SQL。Windows 及其他非 Linux、非 Darwin 平台仍走这条拒绝。

错误分类保持分开：

- `sql_result_too_large`：已确认上限后的 Go `out of memory` 文本、kqueue `NOTE_EXIT_MEMORY`，或父进程/worker 自己的足迹停止。不返回行。
- `sql_worker_failed`：SIGSEGV、外部 SIGKILL、协议损坏、其他异常退出。
- `sql_worker_unavailable`：进程没起来、退出 125，或 Darwin 上看见 worker 后无法继续读取足迹。
- 取消仍是 context 超时，优先于上面几类。

## 不做什么

不重做 Linux `close_range` 和 B1 完整性测试。不把 SQL worker 接到 Python `Sandbox.Run`。不改只读 SQL 语义和结果 JSON 预算。不做 unlink-open 的 `proc_pidfdinfo`。不关闭 W4，不开始 W5 RAG。

## 验证

macOS CI 的 `macos-sql-worker` 运行 `go test ./core/sqlx/` 和 `go test ./core/agent/`。其中 Darwin 用例覆盖：父进程不执行的只读查询、写语句和 ATTACH 拒绝且文件不变、登记路径被替换时 `data_busy`、超时后进程消失且下一次查询恢复、worker 环境只有三条允许变量、超过 1 GiB 物理足迹时 `sql_result_too_large` 且不返回行、父进程被杀死后 worker 消失、外部 SIGKILL 与运行时 OOM 文本不是同一类。Agent 循环里有一次真实 `SELECT 42`，以及一次超限后连续参数失败计数不被清零。

Linux 交叉编译只能证明 Darwin 文件能编过。`proc_pid_rusage`、kqueue 和足迹停止要看 macOS 作业。本仓库的 Linux 测试只能覆盖 procargs 解析和既有 Linux worker。

## 限制

足迹采样不是内核瞬时配额，超调量取决于调度。物理足迹和 `ru_maxrss`、`proc_pidinfo` 驻留字节不是同一个计数器。父死监视不覆盖 shim。Darwin worker 仍会继承 fd≥3。临时文件没有磁盘配额。这些都不是“1 GiB 虚拟地址空间已经在 macOS 上生效”。

## 面试问答

为什么不用 `RLIMIT_AS`？因为在 Darwin 上它和 `RLIMIT_RSS` 是同一个号，而且内核不强制；设成功会假装有硬上限。为什么不用 jetsam？因为 `memorystatus_control` 要私有 entitlement，CI 上的普通用户会得到 `EPERM`。为什么字节数仍是 1 GiB？因为把足迹阈值改得更大就是在放大预算；换的是计量对象，不是数字。为什么外部 SIGKILL 不是超限？因为崩溃和超时杀进程都会变成 SIGKILL，只有足迹观测或明确的运行时 OOM 文本才能进 `sql_result_too_large`。为什么失去采样要失败关闭？因为看不见足迹时继续查询等于把上限拿掉。
