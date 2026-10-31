// Package sqlx 提供当前工作区 analytics.db 的只读查询。
//
// NewTool 是模型 SQL 的执行入口。Linux 下每次查询启动独立 worker，在第二次 exec
// 之前设定 RLIMIT_AS=1 GiB；这包括 Go 运行时的虚拟地址预留，不是 RSS 或 SQLite 堆配额。
// 超限导致 SQLite NOMEM 或明确的 Go 运行时 OOM 时，返回 sql_result_too_large，不返回部分结果。
// timeout、sql_worker_failed（崩溃/协议损坏）和 sql_worker_unavailable（启动失败）分别处理。
// 不支持隔离的平台失败关闭，不退回父进程查询；macOS 实现在 W4 处理。
//
// 父进程解析结构化 ready/done 协议，核对 PID、内存上限和错误码；从 /proc 和 wait4
// 收集实际峰值。取消时杀整个进程组并 Wait，设置父进程退出时杀 worker，不遗留僵尸。
// worker 的环境不继承 API Key。第二次 exec 前关闭除 stdin/stdout/stderr 以外的
// 文件描述符；只设置 CLOEXEC 不足以挡住父进程已经打开且未设该标志的 fd。
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
