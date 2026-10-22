# ADR 0006：Linux Sandbox.Run 的单任务执行边界

## 状态与范围

实现与验收中（2026-10-23），对应 #41。沿用 ADR 0005 的 Landlock + seccomp 主方案。本轮实现真实 `Sandbox.Run`，数据导出业务、`run_python` 工具和完整评测分别由 #42–#44 验收。

## 决策：先提供严格单任务 profile

当前 Linux amd64 后端要求非 root、无 effective/permitted capabilities 的调用用户，只允许一个不可信 Python 进程、一个执行线程。不把宿主特权交给不可信代码；条件不满足返回 sandbox_unavailable。`Limits.NProc` 仅接受 0（平台默认）或 1，超过 1 明确拒绝为 `ErrInvalidSpec`；不把 PRD §6.2 中待平台实测的进程数 64 建议值写成已实现能力。产品已对照 Issue/接口/PRD 核对这一交付范围。

原因是进程组不等于执行树：代码能 setsid/setpgid 脱组，fork 又会清除子代 Pdeathsig。仅 killpg 或采样后代无法证明零孤儿进程，RLIMIT_NPROC 也不是当前宿主上的单次运行配额。PID namespace 或受委派 cgroup 可以提供更强多进程边界，但会增加本机/runner 支持前提与分发成本；当前不提供需要这些机制的多进程模式。

因此内核直接拒绝 fork/vfork/clone/clone3（包括创建线程）、setsid/setpgid、跨进程信号、ptrace/process_vm/pidfd 等路径，并阻止清除父死与策略设置。设置 BLAS/OMP 等线程数为 1 用于运行时兼容；它们不能替代内核限制。多进程和多线程分析不是当前支持范围，后续若放开需重新验证树级预算与回收。

## 执行链与生命周期

可信 guardian 接收有界规格，创建专用目录，启动 launcher。launcher 在固定 OS 线程上设置限制、清理非必要 FD、安装 no_new_privs、Landlock 和 seccomp，再 exec Python。代码与输入只读；只有 out/tmp 可写。环境由允许项构造，不从宿主继承 API Key 或自定义导入路径。

guardian 不执行用户 Python；它负责监测唯一不可信进程、等待、产物复制和清理。应用正常取消或突然退出时，控制管道 EOF 触发终止与 Wait。Python 的 Pdeathsig 处理 guardian 意外退出，创建线程固定至子进程回收，避免将 OS 线程退出误当作应用退出；恶意代码不能通过 prctl 清除此设置。不能将这两个层次与父死信号自动递归继承混为一谈。

顺序固定为：停止并回收执行进程 → 有界收集并验证允许产物 → 将字节复制进 Result → 清理执行目录 → 返回。#42 复用这一基础收集，不会读取已经删除的临时路径。

## 资源口径

- Memory 保留 RSS 含义，只统计唯一不可信 Python。目标每 10ms 采样 `/proc/PID`，超过预算终止。报告观察峰值、实际最大采样间隔和累计 CPU 等 Usage；这是采样超限终止，不是瞬时内核 RSS 配额，也不承诺固定超调字节。RLIMIT_RSS 在本机独立实验证明不会提供所需硬限制；RLIMIT_AS/cgroup memory 的口径均不能冒称 RSS。
- CPU 是累计 CPU 时间预算，默认 10 秒、请求硬上限 30 秒。单任务 profile 下不存在可逃到短命后代的累计时间；进程统计加 rlimit 兜底，不宣称控制“1 核速率”。
- 墙钟默认 30 秒、请求硬上限 60 秒，取消与到期都要终止并回收。
- RSS 默认 512 MiB、请求硬上限 1 GiB；请求大于后端硬上限明确拒绝。可信 guardian 的开销单独存在，不混称为不可信代码 RSS。launcher 累计 CPU 在交接时记录为 baseline，从执行统计中扣除；Wait RSS 含启动开销需单独披露。
- stdout/stderr 各 64 KiB，代码最多 1 MiB、输入总量最多 16 MiB；工作目录总量 64 MiB/1024 项使用采样终止，单文件限制与总量采样分别说明。产物单件 5 MiB、合计 20 MiB/16 件。全进程同时最多两个 Run，等待配额须尊重 context。

以上为冻结的实施/验收目标，最终限制和实际测量以代码、测试与本轮 PR 为准；不能将目标表述为尚未跑过的测试结果。

## 运行时与路径边界

`Runtime.PyRoot` 统一指标准库目录；`pyrt.Runtime.Base.Stdlib` 对应此字段，不能把 `/usr` 前缀整体放行。解析解释器、stdlib、venv、ELF loader 与递归共享库闭包，精确放行所需只读文件/目录；不执行 ldd，不默认开放 home、工作区或系统目录整树。解释器/loader 必需闭包不完整返回 runtime_unavailable，不扩大权限兜底。宿主本就缺库的可选扩展记录 RuntimeWarnings，不阻断与它无关的普通任务；导入该扩展仍会失败，不能将其承诺为可用。实际 pandas/Agg 必须实测。

`ExtraReadOnly` 来自可信调用方已经完成的 Policy/Approver 决策，Sandbox 自身不新增 UI，也不接受用户代码申请扩权。Landlock 不隐藏全部元数据、不撤销已有 FD；关闭继承 fd 与限制进程间影响必须单独验证。内核本身的漏洞不在此隔离保证内。

## 验收计划与证据位置

开发专项和独立测试应覆盖真实成功/异常及恢复、宿主权限正对照、只读边界、socket、继承文件/联网 FD、信号、创建进程/线程/脱组的拒绝、RSS 与大虚拟地址预留的区别、CPU 忙循环与睡眠、超时/取消、父应用退出后的 Python/guardian 回收及目录清理、并发互不影响、总体输出与磁盘预算。

关键防护移除后必须在对应断言失败，不能将编译错误或挂死当作成功杀变异。通过真实 Agent loop 的测试适配器验证底层执行结果与资源错误反馈；它不等于 #43 的生产 run_python 工具已经完成。

CI 必须用已有哈希锁定依赖形成真实 venv，验证 pandas 计算与 matplotlib Agg PNG，在本 profile 中可用。普通单测不联网；显式真实包验收开关启用时，缺依赖/不兼容不能 skip。最终提交 CI、reviews/comments 正文、三角色结论和笔记同步均是本轮出口。

## 官方依据

- [Landlock](https://docs.kernel.org/userspace-api/landlock.html) 与 [Seccomp](https://docs.kernel.org/userspace-api/seccomp_filter.html)。
- [Pdeathsig](https://man7.org/linux/man-pages/man2/PR_SET_PDEATHSIG.2const.html)：OS 线程、fork 和凭据变化的语义。
- [close_range](https://man7.org/linux/man-pages/man2/close_range.2.html)：关闭/标记非 stdio FD 与 unshare。
- [getrlimit](https://man7.org/linux/man-pages/man2/getrlimit.2.html) 与 [cgroup v2](https://docs.kernel.org/admin-guide/cgroup-v2.html)：各种预算的统计与作用域需要分别说明。
