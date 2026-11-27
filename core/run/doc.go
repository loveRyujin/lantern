// Package run 存放运行状态、run_steps 和重启恢复。
//
// Recover 是 Core 的重启恢复入口，不打开窗口。工作区 Create/Open 在迁移之后调用它。
// 调用方也可以在 headless 启动时对已经打开的 runs.db 再调一次。
//
// FR-SM-5 在这次扫描里的归类：
//
//   - 最后一步是 executing：运行标为 failed，审计 executing_interrupted，不执行工具。
//   - 运行是 awaiting_approval，且还有未消费的 pending 或 approved 确认单：保持挂起，
//     审计 approval_restored。批准后的执行走 Resume，用确认单里的冻结参数，
//     执行前用 approval.ParamsHash 核对，再交给既有审批状态机 Decide/Consume。
//   - 运行是 running，且最后一步是 done：这是步骤间的明确断点。本入口不调用模型、
//     不执行工具，也不把运行标成 completed。状态保持 running，审计 breakpoint_held。
//     自动续跑模型需要已经持久化的对话，不在这次恢复里重放。
//   - 其余未终结残留（裸 running、created、pending、error、没有可恢复确认单的挂起、
//     未知状态）标为 failed，审计 illegal_residual，不执行工具。
//
// 已终结的运行（completed、failed、cancelled、rejected、expired）不改写。
// executing 优先于 awaiting_approval：步骤已经开始就不能再按挂起去执行一次。
// 挂起和步骤间断点每次扫描都会再记一条审计。已经标成 failed 的运行不会重复记失败。
//
// 进程内活跃的运行不算残留。Resume 和 Agent 循环用 Hold 登记，Recover 跳过登记过的运行，
// 记进 Report.Active。工作区 Open 每次都会跑 Recover（每次提问都会），所以不跳过的话，
// 正在执行的工具会被同进程的另一次提问判成中断。进程启动时登记表为空，Recover 即完整扫描。
//
// 终态保护：Set 和 Resume 内部的状态改写都不会把已终结的运行改到别的状态，
// 被拒绝时返回 *TransitionError（errors.Is(err, ErrTerminal)），runs 和 run_transitions 不变。
//
// Resume 批准后的批准、消费、步骤 executing、awaiting_approval→running 在同一个事务里提交。
// 写事务遇到 SQLITE_BUSY 时整段重跑，有界时间内仍失败就返回 ErrBusy，确认单保持未消费。
package run
