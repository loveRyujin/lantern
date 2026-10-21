// Package sqlx 提供当前工作区 analytics.db 的只读查询。
//
// 语句扫描只看调用方交给工具的这一段文本，是第一层过滤，用来尽早给出友好错误。
// 它看不懂 Go 里用 fmt.Sprintf、变量或 builder 拼出来再送进连接的 SQL。
// 这是已知限制，不能把它当成安全边界。
//
// 真正的只读保证是连接本身：以 mode=ro 打开，并在每条连接上打开 query_only，
// 同时拒绝 ATTACH。执行之后再核对主库文件哈希；执行前已有的 -wal 哈希不变，
// 执行前没有 -wal 的，执行后也不能出现。
package sqlx
