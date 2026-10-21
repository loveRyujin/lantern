// Package sqlx 提供当前工作区 analytics.db 的只读查询。
//
// 语句扫描只看调用方交给工具的这一段文本，是第一层过滤，用来尽早给出友好错误。
// 它看不懂 Go 里用 fmt.Sprintf、变量或 builder 拼出来再送进连接的 SQL。
// 这是已知限制，不能把它当成安全边界。
//
// 真正的只读保证是连接本身：以 mode=ro 打开，并在每条连接上打开 query_only，
// 同时拒绝 ATTACH。执行之后再核对主库文件哈希；执行前已有的 -wal 哈希不变，
// 执行前没有 -wal 的，执行后也不能出现。-shm 不参与核对，它只是 WAL 的共享内存索引。
//
// 旁边没有 -wal 时用 immutable=1 打开，避免新建 -wal。这次打开不加锁。
// 导入、种子或迁移还在写这个库时，查询结果可能是旧的。
//
// 连接只打开业务库。sensitive.db 不能靠路径、符号链接、ATTACH 或 URI 参数进来。
// SELECT sqlite_master 可以，看到的是业务库自己的表。
package sqlx
