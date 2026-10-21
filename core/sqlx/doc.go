// Package sqlx 提供当前工作区 analytics.db 的只读查询。
//
// 语句扫描只看调用方交给工具的这一段文本，是第一层过滤，用来尽早给出友好错误。
// 它看不懂 Go 里用 fmt.Sprintf、变量或 builder 拼出来再送进连接的 SQL。
// 这是已知限制，不能把它当成安全边界。
//
// 真正的只读保证是连接本身：以 mode=ro 打开，并在每条连接上打开 query_only，
// 同时拒绝 ATTACH。不加 immutable，读取会等写入者的锁。等不到锁，返回可重试的
// data_busy，不把行交给模型。
//
// 执行之后再核对主库文件哈希；执行前已有的 -wal 哈希不变，执行前没有 -wal 的，
// 执行后也不能出现。任何一项对不上，丢掉这次结果，同样返回 data_busy。
// -shm 不参与核对，它只是 WAL 的共享内存索引。
// Open 之后才出现的 -wal 也返回 data_busy：旧连接可能还停在打开时的快照上。
//
// 结果除了行数上限，还有 1 MiB 的总字节上限。多行各自没超限、加在一起超了，
// 就停下并标 truncated，原因是字节。单独一行超过 1 MiB 时不截断这一行，
// 整次查询失败，错误码是 sql_result_too_large，已读出的行也不返回。
// 这个错误交回模型，建议少选几列或用 substr。它不计入连续失败，也不把计数清零。
// 1 MiB 只约束最终返回的每一行。表达式的中间值单独放到 4 MiB，不按列数再收紧。
// 不设置 SQLITE_LIMIT_COLUMN：它在加载表结构时也生效，宽表会让整个库打不开。
// 结果列数在准备语句之后、第一步之前检查，最多 32 列，再多就以 sql_rejected 交回模型，不计入连续失败。
// 每一列先看写进 JSON 的长度，再决定要不要拷贝。一行超过 1 MiB 就停，不返回已读出的行。
// TEXT 按原始字节计。BLOB 在 JSON 里是 base64，1 MiB 按编码后的长度计，不按原始字节。Go 侧停在 1 MiB。
// 只读连接设置 temp_store=FILE，并把 cache_size 留在 256 KiB，让排序多出来的行写到临时文件。
// 临时文件在系统临时目录，不在业务库旁边，也不进文件哈希。
// 不设置 sqlite3_hard_heap_limit64，也不设别的进程级堆上限，否则工作区、审计和敏感库写入会一起被卡住。
// 已知风险：子查询、物化 CTE、GROUP BY、UNION、IN 列表和 ORDER BY 会让 SQLite 同时握住很多中间值，内存没有硬上限。
// 超时是现在的兜底。真正的修复是 W3（issue #38）：把 run_sql 放进单独的子进程，用 RLIMIT_AS 或 cgroup 限制内存。
// SQLite 若仍返回 NOMEM，按 sql_result_too_large 交回。
// 引号里的 pragma_* 表值函数和 PRAGMA 一样拒绝，按解析后的标识符判断，不在原文里搜子串。
//
// 连接只打开工作区登记的那一个业务库。要打开的路径和登记路径都先做绝对化和 EvalSymlinks，
// 再比较；文件身份用 os.SameFile 再对一次。指向别处的符号链接、改名副本和硬链接都打不开。
// sensitive.db 也不能靠 ATTACH 或 URI 参数进来。
// SELECT sqlite_master 可以，看到的是业务库自己的表。
package sqlx
