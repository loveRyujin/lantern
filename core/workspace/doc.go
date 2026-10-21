// Package workspace 管理工作区目录布局，并打开工作区里的 SQLite 库。
//
// root 是应用数据目录。每个工作区是其中的一个子目录：
//
//	<root>/workspaces/<id>/
//	    kb.db          文档、切片、向量
//	    analytics.db   业务表，users 只有脱敏列
//	    sensitive.db   手机号和邮箱原文；确认导出前不创建、不打开
//	    runs.db        运行、步骤、事件、确认单、审计
//	    exports/       确认后的导出文件
//	    tmp/           沙箱临时目录
//
// [Create] 和 [Open] 只打开 kb、analytics、runs。sensitive.db 不在启动路径上。
// 用户确认导出后，调用 [Handle.ConfirmExport] 拿到 [ExportGrant]，再交给
// [Handle.OpenSensitive] 才会创建并打开这个库。凭证是一次性的，绑在签发它的工作区上；
// 零值、伪造值、别的工作区的凭证、已经用过的凭证都拿不到连接。
//
// 库都用 modernc.org/sqlite，并以 WAL 打开。Core 只拿 [Handle]，按路径分别打开，
// 没有“共用一个库再过滤”的做法。每条由本包打开的连接（含确认后的 sensitive）都把
// SQLITE_LIMIT_ATTACHED 设为 0，并安装 authorizer 拒绝 ATTACH。analytics 是会执行
// 外部 SQL 的那条连接，因此它打不开 sensitive.db。
package workspace
