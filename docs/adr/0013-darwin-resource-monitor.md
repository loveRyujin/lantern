# ADR 0013：Darwin 内部 CPU/RSS/Wall 在线监测

状态：合同冻结中；CPU 跨 exec 计数口径等待真实原生前检。关联 Issue #58，依赖 #56 / ADR0012 已完成的 main08a9cc5。

## 目标与范围

在现有 `Supervisor.Run` 增加一个 sealed Go 资源 helper 与值类型预算。只验证 CPU/RSS 在线采样与独立 Wall，不接任意代码、SQL、Python Runner、Sandbox.Run 或 Session；磁盘、已 unlink 文件、NPROC、进程 profile、stdout 总量和全局并发仍是业务出口前的后续依赖。旧 GoLifecycle 的18项不启用本轮计量，继续回归。

不新增 Start/Wait/Close handle、调度器、运行时框架或生产 CounterProbe。原生 CPU 前检使用测试私有 fixture，以现有 safe spawn 的 SETEXEC 验证同 PID/birth 计数边界，不改变生产 helper 枚举。

## 预算提案与有界性

可信内部请求逐字段零值采用默认：CPU1s、RSS128MiB、Wall3s。请求上限分别为3s、512MiB、5s；负 duration 与超限拒绝，不能静默 clamp。这是内部固定 helper 的参数，不是未来模型或业务默认。资源 helper 只允许 NoGate，仍使用完整 BOOT/registered/armed/GO 握手。

目标采样间隔10ms；保留原始有效/失败样本、实际最大 gap、首次与最后样本窗口、触发时点与观测 overshoot。样本最多1024条，溢出明确失败，不能静默丢峰值。连续读取失败从首个失败起50ms后触发 monitor_error；恢复则清连续失败窗并保留历史失败。该期限是软件触发条件，不是操作系统实时调度保证。

## 为什么由宿主采样

Host.Run 的主 select 持有已登记身份并采样 libproc，原始样本直接进入同进程 Result。Guardian 只接受受限的 typed STOP 原因并保留真实 worker Wait/退出码；原 CANCEL 语义不变。这样无需把最多数十KB样本塞入4KiB协议帧，也无需新遥测流或读取磁盘日志作控制。宿主死亡仍由已验证 guardian 生命周期路径收回任务；死亡的调用者不会伪造资源 Result。

预算配置不修改宿主的 rlimit、环境、cwd 或既有 FD flags，不添加 rlimit CPU/AS 后备。CPU/RSS 都是在线采样触发，不宣称瞬时硬 quota；不同原因和工作退出事实分别记录。

## 三种计量不得混用

CPU 使用 PROC_PIDTASKINFO 的当前 Mach task 全线程累计 user+system，按实测 Mach timebase 换算，不能误用仅存活线程字段或把累计CPU称一核速率。Apple一手源码显示 SETEXEC 创建新 task，而 task recount 是否完整转移不能由注释推断；必须记录 pre-exec burn 后与同 PID/birth post-exec 暂停期间的实际原始计数。未出结果前不声称覆盖整启动链，也不实现 baseline 扣除算法。

RSS 是驻留内存采样峰，Virtual 另列；未触页的大映射不能按虚拟尺寸误触发 RSS。若收集 Wait4 返回峰，须另证其作用域、单位与启动链含义，不能替代在线触发证据。本轮可不收集这项，避免无必要扩大合同。

Wall 从明确 GO 授权起，到 guardian 对任务实际 own Wait 返回时观察的单调时钟终点；GO与Wait使用同一跨进程时钟口径，Unix 时间仅供审计。receipt传输、宿主收包与清理延迟另列，不能算成任务执行时间。准备和收尾仍有现有生命周期期限。

## 验证与面试重点

正常短负载、CPU busy、多线程CPU、低CPU sleep、实际触页内存与 untouched mmap 均为固定有界负载。CPU/RSS/Wall 必须由对应在线路径在自然结束前检出，保留真实原因、sample、signal/Wait/absence 与目录清理；取消、外部终止、监测失败和资源超限不得互相伪装。至少一次超限/取消后恢复与两个并发调用隔离，宿主配置不变。

独立移除CPU/RSS采样终止与Wall动作，目标行为必须真实违反承诺；编译错误、前检失败、外层超时或更早其他预算终止不能冒充检出。最终专项、全量、平台、真实macOS CI、review正文、合并/main与lantern-notes保持同轮闭环。

面试追问：为何先做exec计数前检？因为API名字和PID连续不能证明内核计数器连续。为何不把样本放协议receipt？因为协议预算和资源观测数组的上界不同，同进程宿主持有样本能减少新协议复杂度。为何用采样仍保留不可用的业务出口？因为采样盲区和未完成的其它资源/进程合同不能靠限制披露变成任意代码安全证明。
