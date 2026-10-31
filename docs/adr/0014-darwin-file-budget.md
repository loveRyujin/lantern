# ADR 0014：Darwin 私有目录的候选文件预算

状态：已采用为计量决策；Darwin 原生六负载、独立恢复与变异证据以本轮 PR 的 macOS CI 为准，本文不把 Linux 扫描测试写成 Darwin 验收。关联 Issue #60。依赖 #58 / [ADR 0013](0013-darwin-resource-monitor.md) / [PR #59](https://github.com/loveRyujin/lantern/pull/59) 的 main `e6eb09f`，以及 #52 / #56 的密封固定 helper 与单故障监督。不关闭 W4 里程碑。

## 目标与范围

在现有 `Supervisor.Run` 的 `GoResource` helper 上增加一组固定文件负载，只回答一件事：私有 `out/tmp` 树里，哪一种目录扫描可以当成本轮的候选字节预算，以及它看不见什么。

不交付 Darwin Runner、插件、业务工具、进程 profile / NPROC、stdout 封顶、全局并发、SQL worker、Python Runner / Session、签名生产链、真实 macOS eval，也不做通用 quota 框架。生命周期 helper 不启用本轮计量。请求不能携带任意路径；工作目录只由监督器在运行前创建，helper 只能对这个目录做相对 `openat` / `mkdirat` / `linkat` / `renameat` / `symlinkat` / `unlinkat`。

## 决策

候选字节预算是 **unique-inode 的实际分配字节**：对私有根做 `lstat` / `WalkDir`，不跟随符号链接，每个普通文件 `(dev, inode)` 只计一次。Darwin 上这个数是 `getattrlist(FSOPT_NOFOLLOW)` 的 `ATTR_FILE_DATAALLOCSIZE`。其它 Unix 上是 `st_blocks * 512`。`st_blocks * 512` 在 Darwin 报告里另记为 `stat_blocks`，不是 Darwin 的预算。

三条轴分开记录，不能互相代替：

| 轴 | 算法 | 本轮角色 |
| --- | --- | --- |
| `logical_named` | 每个普通文件目录项的 `st_size` 之和 | 不是预算。硬链接按名字再计一次 |
| `logical_unique` | 每个普通 inode 的 `st_size` 只计一次 | 不是预算。稀疏文件会远大于实际占用 |
| `allocated_bytes` | Darwin：`ATTR_FILE_DATAALLOCSIZE`；其它 Unix：`st_blocks * 512`。每个普通 inode 一次 | **候选字节预算** |
| `stat_blocks` | 每个普通 inode 的 `st_blocks * 512` | 对照，不是 Darwin 预算 |
| `entries` | 扫描根以外的现存目录项，含目录和符号链接 | 独立名字上限，不是字节预算 |

符号链接只计目录项，不 `stat` 目标，不走出去。硬链接共享一个 inode，不能当成两份字节。rename 后仍是同一 inode 的新名字，本轮是时点快照，不是累计路径账本。

目录扫描看见的体积 **不是** 全部打开文件的磁盘消耗。`open → write → unlink → 继续 write → close` 必须单独记录：写出的字节、fd 的 dev/ino 与 nlink、扫描可见量、关闭前和关闭后。候选扫描看不见已 unlink 但仍打开的部分时，结论固定为 `unresolved-unlinked-open`。清理成功只说明 guardian 在结束后删掉了私有根，不能写成运行中预算已被保护。`budget_protected_during_run` 在这条结论上保持 false。

## 为什么不是另外两条轴

逻辑长度会把 1MiB 稀疏文件（末尾 1 字节）算成超过本轮请求上限的占用，也会把硬链接的第二个名字再加一遍。名字数量能限制目录项，但 16 个空文件可以在字节几乎为零时先撞上名字上限，反过来少量大文件可以在名字很少时用完字节。所以字节比较和名字比较是两个固定判断，共用停止原因 `file_limit`，用 `FileTriggerAxis`（`allocated_bytes` 或 `entries`）分开。独立变异只拿掉字节比较那一条语句；名字上限必须仍然单独触发。

不使用 `RLIMIT_FSIZE`。它限制的是单次写的逻辑长度，不是这次私有树的分配量，也看不见 unlink 之后的 fd。不沿用旧 spike 里按 `st_size` 走目录的 `diskUsage`。

## 采样、上冲和上限

沿用 ADR 0013 的宿主采样：目标间隔 10ms，样本最多 1024，连续读失败 50ms 后 `monitor_error`。文件比较在 CPU/RSS 之前，避免外层预算抢先终止。这是采样触发，不是内核瞬时 quota。越过预算的那一次样本记为 overshoot，允许最多大约一个采样间隔的继续写入。

请求字段零值 **不会** 给文件预算填默认值（CPU/RSS/Wall 的默认只属于那三项）。文件负载必须显式给出 `FileBytes`。只有 `file-entries` 可以并且必须给出 `FileEntries`。其它资源负载带上文件字段会被拒绝。

请求上限：`FileBytes` ≤ 256KiB，`FileEntries` ≤ 64。helper 自己的写入上限更低且调用方不能抬高：单块 16KiB，单文件生长 128KiB，6×32KiB，16 个空文件，unlink 可见 16KiB 后再对已打开 fd 写 96KiB，稀疏逻辑长度 1MiB。外层 CPU 3s / RSS 256MiB / Wall 3s（取消用例 Wall 5s）只是护栏。停止必须能证明不是这三项先触发。

失败和取消之后，guardian 仍负责 `cleanRoot`。测试不自己 `RemoveAll` 工作根。下一次受控任务必须使用新的根，并且不继承上一次的样本、nonce、pid 或证据目录。宿主 cwd、环境和 rlimit 保持不变。

## 已观察与尚未观察

Linux 上的 `scanPrivateTree` 测试使用真实临时目录：指向目录外文件和子目录的符号链接不计入字节；硬链接只增加 `logical_named`；rename 保持同一 inode 和同一 allocated；稀疏文件的逻辑长度大于 allocated；unlink 后目录扫描回到 unlink 前的占用，fd 仍可读到更大的 `st_size`，关闭后 `Fstat` 失败。

[CI 41195197677](https://github.com/loveRyujin/lantern/actions/runs/41195197677) 的 macOS 15.8.2 / build 24H232 / arm64 / Go 1.27.3 跑完了六个负载。`file-small`、`file-multi`、`file-entries`、`file-grow`、`file-unlink` 通过。unlink 的结论是 `unresolved-unlinked-open`，清理执行者是 guardian，`candidate_meters_unlinked_open` 为 false。`file-edges` 的硬链接、rename 和两个符号链接计数正确（entries 8、inodes 3、symlinks 2，`logical_named` 比 `logical_unique` 多 16KiB），但 `ftruncate` 到 1MiB 再在末尾写 1 字节后，`st_blocks * 512` 与逻辑长度同为 1081344。APFS 把这段跨度实际分配了，候选预算因此以 `allocated_bytes` 触发 `file_limit`。这不是“逻辑长度被误当成预算”的成功证明，而是这个构造在 APFS 上并不稀疏。

随后的构造在扫描可见之前对未写的 4KiB 对齐区间调用 `F_PUNCHHOLE`，Darwin 候选改读 `ATTR_FILE_DATAALLOCSIZE`。这一改动是否让 `file-edges` 在逻辑长度超过 256KiB 的同时保持实际分配低于预算，要等下一次 macOS 报告。已 unlink fd 的 `st_nlink == 0` 已在上述通过的 `file-unlink` 里观察到。

## 不支持的路径

- 不把目录扫描称为打开文件的全部磁盘消耗。
- 不根据 helper 自报的已写字节去杀 unlink-open；那会把“看不见”伪装成“已限额”。
- 不提供任意路径、跟随符号链接、或按路径累计的账本。
- 不把最终 `WorkRootClean` 写成运行中保护。
- 不接入业务后端，不声明硬磁盘 quota。

## 下一件最小任务

若本轮确认候选扫描看不见 unlink-open，下一轮只做同一 helper 的只读观察：对仍打开的已 unlink fd 调 `proc_pidfdinfo` / `PROC_PIDFDVNODEINFO`，看 `st_blocks` 在 `nlink == 0` 时是否仍然可见。不做通用 quota 框架，不由此自动终止，先记录能不能计量。

## 面试追问

为什么候选是 `st_blocks` 而不是 `st_size`？因为稀疏文件的逻辑长度会把空洞算进预算，硬链接的逻辑长度还会按名字重复；分配块按 inode 计一次，更接近这次私有树实际占用。为什么清理成功仍叫未决？因为预算要在运行中起作用，扫描在 unlink 之后已经是 0，而 fd 仍能继续写；结束时删目录不能补上这段盲区。为什么变异必须让 helper 自然退出 0？因为编译失败、前检失败、外层超时或 CPU/RSS/Wall 先触发都不能证明被拿掉的是字节比较这一条。
