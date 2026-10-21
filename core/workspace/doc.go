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
// 用户确认导出并落成 runs.approvals 里的一条已批准记录之后，调用
// [Handle.ConfirmExport] 才会签发 [ExportGrant]。记录必须存在、决定为批准、
// 动作是 export_data、工作区和导出对象与本次调用一致、未过期，且没有用过。
// 任意字符串签不出凭证。凭证再交给 [Handle.OpenSensitive] 才会创建并打开这个库。
// 凭证绑在签发它的工作区上，同一张确认单和同一张凭证都只能成功打开一次。
// 拒绝时的错误码是 export_unconfirmed。
//
// 库都用 modernc.org/sqlite，并以 WAL 打开。Core 只拿 [Handle]，按路径分别打开，
// 没有“共用一个库再过滤”的做法。每条由本包打开的连接（含确认后的 sensitive）都把
// SQLITE_LIMIT_ATTACHED 设为 0，并安装 authorizer 拒绝 ATTACH。analytics 是会执行
// 外部 SQL 的那条连接，因此它打不开 sensitive.db。
//
// [Remove] 删除一个工作区目录。目录不存在时什么也不做。符号链接和逃出 workspaces
// 的路径返回 [ErrPathEscape]，不会跟着链到外面删文件。[Install] 在全部检查通过之后，
// 才把另一份数据目录里已经建好的工作区换进来。
package workspace
