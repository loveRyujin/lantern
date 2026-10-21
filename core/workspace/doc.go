// Package workspace 管理工作区目录布局，并分别打开 kb、analytics、sensitive、runs 四个库。
//
// root 是应用数据目录。每个工作区是其中的一个子目录：
//
//	<root>/workspaces/<id>/
//	    kb.db          文档、切片、向量
//	    analytics.db   业务表，users 只有脱敏列
//	    sensitive.db   手机号和邮箱原文
//	    runs.db        运行、步骤、事件、确认单、审计
//	    exports/       确认后的导出文件
//	    tmp/           沙箱临时目录
//
// 四个库都用 modernc.org/sqlite，并以 WAL 打开。Core 只拿 [Handle]，按路径分别打开，
// 没有“共用一个库再过滤”的做法。analytics 是会执行外部 SQL 的那条连接：打开时把
// SQLITE_LIMIT_ATTACHED 设为 0，并安装 authorizer 拒绝 ATTACH，因此这条连接打不开
// sensitive.db。同一套打开路径上的另外三个库也装上同样的限制。
package workspace
