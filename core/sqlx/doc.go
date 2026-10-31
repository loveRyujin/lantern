// Package sqlx 提供当前工作区 analytics.db 的只读查询。
//
// NewTool 是模型 SQL 的执行入口。Linux 与 Darwin 每次查询都启动独立 worker，
// 数值上限都是 1 GiB，但机制不同。Linux 在第二次 exec 之前设定 RLIMIT_AS=1 GiB，
// 覆盖 Go 运行时的虚拟地址预留，不是 RSS 或 SQLite 堆配额。Darwin 不使用 RLIMIT_AS：
// 它和 RLIMIT_RSS 是同一个资源号，setrlimit 返回 EINVAL，内核不拿它限制 mmap。
// memorystatus_control 还要求私有 entitlement，普通进程拿不到 jetsam 致命上限。
// Darwin 的停止线是 proc_pid_rusage 的物理足迹（ri_phys_footprint，字节）。worker 和
// 父进程大约每 2ms 采样一次，大于 1 GiB 就结束进程。采样有一个间隔的超调，不是虚拟地址硬顶。
// 发 ready 前读不到自己的足迹，或看见 worker 后持续读失败，都以 125 失败关闭，
// 不退回父进程查询，也不把上限放大。
// 超限（SQLite NOMEM、明确的 Go 运行时 OOM、Darwin NOTE_EXIT_MEMORY、worker 自己的足迹停止
// 或父进程的足迹停止）返回 sql_result_too_large，不返回部分结果。timeout、
// sql_worker_failed（崩溃/协议损坏）和 sql_worker_unavailable（启动失败，或 Darwin 上
// 看见 worker 后无法继续读取足迹）分开处理。Windows 等其余平台仍然失败关闭。
//
// 父进程解析结构化 ready/done 协议，核对 PID、内存上限和错误码。Linux 从 /proc 和 wait4
// 收集峰值；Darwin 把采样到的物理足迹放进同一峰值字段，wait4 的 ru_maxrss 在 Darwin 上是字节，
// 而且只是驻留峰值，不能乘 1024，也不能代替足迹停止线。
// 取消时杀整个进程组并 Wait。Linux 用 Pdeathsig 在父进程死亡时杀 worker；Darwin 没有该调用，
// worker 在第二次 exec 之后用 kqueue 监视父进程退出并杀掉自己的进程组。两种路径都不留僵尸。
// worker 的环境不继承 API Key。Linux 在第二次 exec 前用 close_range 关闭除 stdin/stdout/stderr
// 以外的文件描述符；Darwin 不重复这条路径。
// 此处是 SQL 资源隔离，不替代后续 Python 沙箱的文件系统和网络隔离。
//
// 语句扫描只是提前报错。真正的只读边界为 mode=ro、每条连接 query_only、拒绝 ATTACH，
// 业务库路径白名单和文件身份校验。构造工具后文件被替换会返回 data_busy，需重新打开工具。
// worker 在查询前后核对主库和 -wal 哈希；变化时丢弃结果。-shm 只是索引，不参与哈希。
// 父进程不为核对哈希而打开并关闭业务库，避免释放同进程写连接的 POSIX 锁。
// 哈希流式读取，不把整个数据库拷入父进程内存。
//
// 最终返回 JSON 最多 1 MiB，包括列名、元数据、字符串转义和 base64。单行超限整次失败；
// 多行累计超限则保留完整行并标 truncated。结果列最多 32，单个 SQLite 值最多 4 MiB。
// JSON 关闭 HTML 转义；非法 UTF-8 TEXT 以 EncodedText 标记的 base64 对象无损返回。
// 合法 TEXT 仍是字符串，BLOB 仍是 base64 字符串。完整 JSON 的预算预留少量截断元数据。
//
// Query 是当前进程内的低层读取器，没有中间值总内存限制；仅供 worker 和受信任调用方使用。
// 模型生成的 SQL 必须走 NewTool。temp_store=FILE 和 256 KiB 缓存仍用于降低排序占用，
// 它们不替代进程硬限制。不设置进程共享的 sqlite3_hard_heap_limit64，避免影响审计和工作区写入。
package sqlx
