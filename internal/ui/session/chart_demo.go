package session

import (
	"encoding/json"

	"github.com/loveRyujin/lantern/core/llm"
)

// ChartDemoPrompt 是场景 1 的演示提问。
const ChartDemoPrompt = "2026 年 9 月华东已支付 GMV 是多少？和 8 月比变化多少？画个柱状图。"

// ChartDemoSQL 按城市归到华东，统计 8、9 月已支付订单金额，单位分。
const ChartDemoSQL = "SELECT substr(o.created_at,1,7) AS month, SUM(o.amount_cents) AS paid_cents " +
	"FROM orders o JOIN users u ON u.id=o.user_id " +
	"WHERE o.status='已支付' AND o.created_at >= '2026-08-01T00:00:00Z' AND o.created_at < '2026-10-01T00:00:00Z' " +
	"AND u.city IN ('上海','杭州','南京','宁波','苏州') GROUP BY month ORDER BY month"

// ChartDemoCode 读 run_sql 的结果画柱状图。标签用 ASCII，沙箱里没有中文字体。
const ChartDemoCode = `import os
import matplotlib.pyplot as plt
columns, rows, meta = load_table()
months = [str(r[0]) for r in rows]
yuan = [int(r[1]) / 100 for r in rows]
fig, ax = plt.subplots(figsize=(6, 3.6), dpi=100)
ax.bar(months, yuan, color='#3b82f6')
ax.set_title('East China paid GMV (CNY)')
for i, v in enumerate(yuan):
    ax.text(i, v, f'{v:,.2f}', ha='center', va='bottom')
fig.tight_layout()
fig.savefig(os.environ['OUTPUT_DIR'] + '/gmv_east.png')
change = (yuan[-1] - yuan[0]) / yuan[0] * 100 if len(yuan) == 2 and yuan[0] else 0
print('gmv', ' '.join(f'{m}={v:.2f}' for m, v in zip(months, yuan)), f'change={change:.2f}%')
`

// NewChartDemoRunner 构造场景 1 的演示循环：模型是进程内脚本，工具是本运行真实的 run_sql / run_python。
// 图表字节只留在 analysis 的 Session 里。export_data 仍注册，待确认列表里的旧票据可以照常重开。
func NewChartDemoRunner(root string, waiter *Waiter, analysis *Analysis) *LoopRunner {
	return &LoopRunner{
		Root:        root,
		Waiter:      waiter,
		Tool:        NewExportCounter(),
		NewProvider: chartDemoProvider,
		Demo:        true,
		Analysis:    analysis,
	}
}

func chartDemoProvider(runID string) llm.Provider {
	sqlArgs, err := json.Marshal(map[string]string{"sql": ChartDemoSQL})
	if err != nil {
		panic(err)
	}
	sqlID := "sql-" + runID
	pyArgs, err := json.Marshal(map[string]string{"code": ChartDemoCode, "sql_call_id": sqlID})
	if err != nil {
		panic(err)
	}
	return llm.NewMock(llm.MustNewScript(
		llm.CallTools(llm.ToolCall{ID: sqlID, Name: "run_sql", Arguments: string(sqlArgs)}),
		llm.CallTools(llm.ToolCall{ID: "py-" + runID, Name: "run_python", Arguments: string(pyArgs)}),
		llm.Say("演示回答：GMV 口径以当前工作区《指标口径手册》为准，这里统计华东已支付订单金额。8、9 月对比见下方图表，数值以图和 SQL 为准。"),
	))
}
