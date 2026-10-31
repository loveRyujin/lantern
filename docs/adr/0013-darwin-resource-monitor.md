# ADR 0013：Darwin 内部 CPU/RSS/Wall 在线监测

状态：已采用。关联 Issue #58 / [PR59](https://github.com/loveRyujin/lantern/pull/59)，依赖 #56 / ADR0012 的 main08a9cc5。逐候选验收、变异检查及合并后 main 证据以 PR59 和 [W4-04 笔记](https://github.com/loveRyujin/lantern-notes/blob/main/W4-04-darwin-resource-monitor.md) 的最终记录为准。

## 目标与范围

在现有 `Supervisor.Run` 增加一个 sealed Go 资源 helper 与值类型预算。只验证 CPU/RSS 在线采样与独立 Wall，不接任意代码、SQL、Python Runner、Sandbox.Run 或 Session。私有 out/tmp 的候选字节预算和 unlink-open 盲区由 [ADR 0014](0014-darwin-file-budget.md) 单独冻结；NPROC、进程 profile、stdout 总量和全局并发仍是业务出口前的后续依赖。旧 GoLifecycle 的18项不启用本轮计量，继续回归。

不新增 Start/Wait/Close handle、调度器、运行时框架或生产 CounterProbe。原生 CPU 前检使用测试私有 fixture，以现有 safe spawn 的 SETEXEC 验证同 PID/birth 计数边界，不改变生产 helper 枚举。

## 冻结预算与有界性

可信内部请求逐字段零值采用默认：CPU1s、RSS128MiB、Wall3s。请求上限分别为3s、512MiB、5s；负 duration 与超限拒绝，不能静默 clamp。这是内部固定 helper 的参数，不是未来模型或业务默认。资源 helper 只允许 NoGate，仍使用完整 BOOT/registered/armed/GO 握手。

目标采样间隔10ms；保留原始有效/失败样本、实际最大 gap、首次与最后样本窗口、触发时点与观测 overshoot。样本最多1024条，溢出明确失败，不能静默丢峰值。连续读取失败从首个失败起50ms后触发 monitor_error；恢复则清连续失败窗并保留历史失败。该期限是软件触发条件，不是操作系统实时调度保证。

## 为什么由宿主采样

Host.Run 的主 select 持有已登记身份并采样 libproc，原始样本直接进入同进程 Result。Guardian 只接受受限的 typed STOP 原因并保留真实 worker Wait/退出码；原 CANCEL 语义不变。这样无需把最多数十KB样本塞入4KiB协议帧，也无需新遥测流或读取磁盘日志作控制。宿主死亡仍由已验证 guardian 生命周期路径收回任务；死亡的调用者不会伪造资源 Result。

预算配置不修改宿主的 rlimit、环境、cwd 或既有 FD flags，不添加 rlimit CPU/AS 后备。CPU/RSS 都是在线采样触发，不宣称瞬时硬 quota；不同原因和工作退出事实分别记录。

## 三种计量不得混用

CPU 使用 PROC_PIDTASKINFO 的当前 Mach task 全线程绝对累计 user+system，按实际 Mach timebase 换算，从 GO 起采样并比较预算，不减启动 baseline、不计算 GO 后精确增量、不用 max 抹掉原始计数下降。计数可能包含内核实际保留的 GO 前启动成本，但不宣称精确整个 PID 启动链或所有 exec 前线程的 CPU 都被保留。固定可信 helper 仅一次受控 exec，没有任意再次 exec 入口；不增加 epoch 累加框架。不能误用仅存活线程字段或把累计CPU称一核速率。

RSS 是驻留内存采样峰，Virtual 另列；未触页的大映射不能按虚拟尺寸误触发 RSS。若收集 Wait4 返回峰，须另证其作用域、单位与启动链含义，不能替代在线触发证据。本轮可不收集这项，避免无必要扩大合同。

Wall 从明确 GO 授权起，到 guardian 对任务实际 own Wait 返回时观察的单调时钟终点；GO与Wait使用同一跨进程时钟口径，Unix 时间仅供审计。receipt传输、宿主收包与清理延迟另列，不能算成任务执行时间。准备和收尾仍有现有生命周期期限。

## 验证与面试重点

正常短负载、CPU busy、多线程CPU、低CPU sleep、实际触页内存与 untouched mmap 均为固定有界负载。CPU/RSS/Wall 必须由对应在线路径在自然结束前检出，保留真实原因、sample、signal/Wait/absence 与目录清理；取消、外部终止、监测失败和资源超限不得互相伪装。至少一次超限/取消后恢复与两个并发调用隔离，宿主配置不变。

独立移除CPU/RSS采样终止与Wall动作，目标行为必须真实违反承诺；编译错误、前检失败、外层超时或更早其他预算终止不能冒充检出。最终专项、全量、平台、真实macOS CI、review正文、合并/main与lantern-notes保持同轮闭环。

面试追问：为何先做exec计数前检？因为API名字和PID连续不能证明内核计数器连续。为何不把样本放协议receipt？因为协议预算和资源观测数组的上界不同，同进程宿主持有样本能减少新协议复杂度。为何用采样仍保留不可用的业务出口？因为采样盲区和未完成的其它资源/进程合同不能靠限制披露变成任意代码安全证明。


## CPU 前检：源推断与真实观测

首轮候选 e561e44 的 [PR CI40259674538](https://github.com/loveRyujin/lantern/actions/runs/40259674538) 使用私有测试 fixture，实际 source 为 PR merge6eb69e2/sourceclean，macOS15.7.9/build24G830/arm64/Go1.27.3。锁定执行线程运行有界 burn，**当前task总CPU**实增200334250ns，这不是隔离的单线程CPU测量。实际同PID1384/birth/image，SETEXEC前203892083ns，后207836500ns，暂停100ms后208107000ns，时基125/3，子进程真实Wait0。本次没有观察到计数下降；status=passed仅说明实验有效，不表示资源限制已实现。

Apple macOS15系 XNU 源码中，fill_taskprocinfo使用recount_task_times，task_create初始化task recount，exec路径还会复制thread资源字段。仅凭task初始化与条件rollup曾推测可能重置，但原生锁定执行线程路径未见重置，不能让源码片段推断覆盖真实观测；同样不能由这条路径推出所有已终结启动线程完整保留。raw live-thread字段是现存线程aggregate，不是单个调用线程。正式MultiThreadCPU负载将让一个locked线程提前退出并终结，另一继续，检查本Mach task总累计与现存线程累计的差异，不新增生产模式。

一手参考（用于解释，不声称与运行内核逐字节一致）：[fill_taskprocinfo](https://github.com/apple-oss-distributions/xnu/blob/xnu-11417.140.69/osfmk/kern/bsd_kern.c#L1102)、[task_create](https://github.com/apple-oss-distributions/xnu/blob/xnu-11417.140.69/osfmk/kern/task.c#L1682)、[exec switch](https://github.com/apple-oss-distributions/xnu/blob/xnu-11417.140.69/bsd/kern/kern_exec.c#L5101)、[thread资源复制](https://github.com/apple-oss-distributions/xnu/blob/xnu-11417.140.69/osfmk/kern/thread.c#L1010)。

## 首次资源原生结果与失败修正（尚未终验）

候选0a10048的[CI40910456980](https://github.com/loveRyujin/lantern/actions/runs/40910456980)实际运行source为PR merge `ce53b2d22df4aeffec9061f384979e329d2d21f3`，source clean，macOS15.8.2/24H232/arm64/Go1.27.3。六个固定负载均通过，原始报告保存实际预算、逐样本counter/RSS/virtual与signal/Wait。CPU负载在256107166ns累计CPU时触发250ms预算，RSS触页在78512128bytes时触发64MiB预算，低CPU sleep的GO→Wait为254294208ns并由250ms Wall触发；这些是一次观测，不是最坏延迟保证。MultiThreadCPU的task累计超过2s，并观测已退休线程与live线程累计的差异；untouched实映射256MiB，RSS约10.6MB，不按Go天然巨大虚拟保留尺寸判超限。

该次CI仍失败，不能作为完整验收：并发隔离、持续短读失败与跨Run恢复通过；立即取消的GO→Wait只有7.058917ms，早于10ms首tick，合法只有pre-GO样本。QA曾错误要求每次至少两条样本；修为仅明确取消可无running样本，且对应active Usage必须为零，不能延迟取消来制造采样。资源超限仍需触发前真实running阈值样本。后续增加同Run两次短读恢复再持续失败，核最后连续失败窗而非首个历史失败。

Linux runner默认Python3.14.4超出哈希锁支持的>=3.10,<3.14，被正确拒绝。CI显式选择3.12而不放宽依赖锁。随后旧文件边界测试在正在执行的解释器上得到ETXTBSY，而非PermissionError；VFS自身拒写不能冒充Landlock证据，需用非busy运行时文件的宿主正控与沙箱拒绝作独立证明。修复和最终候选结果仍待确认。三项新资源变异在此失败作业中尚未执行，旧18项及其补充/变异也未运行，不能用之前提交的绿灯补足。

独立代码审查补充修复：任务已自然ownWait、COMPLETE仍在途而Host先选ctxDone时，cancel与空guardian停止原因可以合法并存，不能追加ErrProtocol。实际生产私有决策函数只对资源首因要求matching receipt，并继续拒绝无Host资源首因却出现guardian资源原因；12个语义正反例验证该决策，不称为原生竞态重现。Linux测试修复使用owned、非执行的已批准只读依赖：宿主append-open可用，沙箱read/import成功，write必须PermissionError；解释器的ETXTBSY仅单列VFS拒写。本地真实黑盒通过，CI环境分支仍待新候选。

## 后续候选与证据解释

候选2291880重新通过六负载和三个独立原生用例：同Run短读恢复两次后再持续失读，触发窗从最后一段连续失败计；超限及取消后可恢复调用；并发取消与CPU超限保持身份、目录和原因隔离；宿主cwd/env/rlimit保持不变。CPU/RSS/Wall三项实际动作移除均由真实负载越界检出，worker实际自然退出0而CLI oracle退出1，未借编译失败、CPU前检失败、其它预算或外层超时造红，且快照源恢复。

这些是该候选的证据，不自动代表后续提交或main已经验收。私有collector容量/计数下降/Wait窗口与STOP决策测试属于合成或语义验证；短读为在真实进程中注入遥测失效，不能声称观测到内核实际故障。目标10ms采样间隔的实际gap、触发overshoot与虚拟保留量都应读取raw而非替换成配置值。该实现仍不能作为任意SQL/Python代码的Darwin业务沙箱。
