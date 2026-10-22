# ADR 0005：Linux 沙箱主方案（W3 spike）

## 状态与范围

已采纳（2026-10-22），对应 #40 / PR #46。开发机与 GitHub runner 的主候选均已真实通过；下方初次 CI 证据绑定 `768f8a4`，最终交付仍要求 PR 最终提交的 CI 与独立复核全部通过。仅确定 Linux 隔离方案，不实现 `Sandbox.Run`、`run_python` 或完整 Agent 工具链；#41–#44 仍分别验收。

## 问题与决策依据

PRD 原文偏向 bubblewrap，技术设计曾倾向 Landlock + seccomp；两者均是候选，不能将偏好当作已验证结论。SQL worker 的 `RLIMIT_AS` 仅限制虚拟地址空间，不提供 Python 文件系统/网络隔离，也不等于 `Limits.Memory` 的 RSS 契约。

主方案为 **Landlock + seccomp**，可执行能力探测与真实正反例是启用前提。理由：不依赖额外安装 bubblewrap 或启用 user namespace，可在 `CGO_ENABLED=0` 自身 shim 中安装规则，然后 exec 本机 Python。bubblewrap 保留为对照，不提供静默 fallback：两个后端的可见目录与网络行为不同，不能在策略失败时偷偷切换。

选择不是“零依赖就安全”。Landlock 限制文件访问，seccomp 补足全部 socket family 的禁网目标；进程/资源/信号边界还需 #41。Landlock 按 ABI 探测，不仅判断 uname：本轮要求 ABI >= 3，包含跨目录 REFER 与 TRUNCATE 权限，缺失能力必须失败关闭。未来新增 ABI 权限需明确评估，而不是宣称处理未知权限。

## 可复现验证

运行 `bash scripts/probe-linux-sandbox.sh /tmp/linux-sandbox-report.json`；Linux CI 的 `linux-sandbox-spike` job 安装 bubblewrap/python3，要求 Landlock 主候选真实可用，并保留 JSON artifact。依赖版本、内核、架构、UID/能力、userns 与 ABI 分开记录，unsupported 不能记作隔离 pass。

当前固定 Go 探针仅支持 Linux amd64；其他架构报告 unsupported，不等于其内核没有 Landlock。只执行仓库固定探针，不接受用户提供的代码或命令。探针需要证明：输入可读、runtime 和输入不可写、指定输出可写、宿主 sentinel 拒读、socket family 被拒绝、exec 后策略继承。宿主对照先证明相同 UID 原本有权限；关闭关键防护的反例应令相关验收失败。无法启动固定 payload 不等于成功阻止攻击。

独立测试还用真实宿主 loopback listener 对照网络连通性，并验证 inherited fd 的限制。探针报告不代替完整生产沙箱的逃逸测试集。

### 开发机独立实测

Linux `7.2.8-9-omarchy`、x86_64、UID 1000、CapEff=0；user namespace 上限 61623，unprivileged_userns_clone=1。bubblewrap 0.13.1、普通非 setuid 二进制，unshare user/net/mount/pid 成功；Landlock ABI 10。

独立黑盒对照：无隔离可读宿主 sentinel、改写用户拥有的 input/runtime，并连接真实宿主 TCP listener。Landlock + seccomp 后 input 读/output 写成功，宿主读与 input/runtime 写返回 EACCES，socket family/loopback 返回 EPERM。bubblewrap 最小挂载下 input/runtime 写返回 EROFS，宿主 sentinel 不存在，宿主 loopback 连接失败；但 IPv4/IPv6/UNIX/NETLINK/socketpair 仍可创建。因此 namespace 隔离网络不等于“拒绝所有 socket”，若采用 bubblewrap 也仍需 seccomp 满足已决契约。

### GitHub runner

[初次 PR CI](https://github.com/loveRyujin/lantern/actions/runs/39804910985) 的 `linux-sandbox-spike` JSON artifact（`linux-sandbox-spike-39804910985`，head `768f8a4`）实测：Linux `6.17.0-1022-azure`、x86_64、UID 1001、CapEff=0，Landlock ABI 7；parent NoNewPrivs=0 / Seccomp=0，worker NoNewPrivs=1 / Seccomp=2，主候选 21 项策略检查及 13 项宿主对照通过。

同一 runner 上 bubblewrap 0.9.0 已安装、非 setuid，但初始化网络命名空间时返回 `bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted`，报告为 **unsupported**，没有运行 Python payload，不能算隔离通过。不修改宿主安全配置绕过这一限制。这是实测失败原因，不臆测归因于某个 AppArmor 配置。主方案不依赖 user namespace；因此这项结果支持选择 Landlock + seccomp。

最终 head 的四个 CI job 仍必须通过，报告随每次 CI 保存；初次 runner 证据不能替代最终检查。生产资源/逃逸用例尚未接入，不能将固定探针当作 #44 的真实完整评测。

## 独立验收与变异

修复后的固定探针在本机严格模式再次通过：Landlock 21 项检查 / 13 项宿主对照，bubblewrap 12 项检查 / 4 项宿主对照。独立测试在临时源码副本分别移除 Landlock、seccomp：前者在宿主读/只读目录写断言失败（0.208 秒），后者在各 socket family/socketpair 等目标断言失败（0.194 秒）；均正常编译、返回完整 JSON，没有将超时算作成功杀变异。bubblewrap 副本加入 `--share-net` 后可连接真实宿主 listener，禁联通断言失败。

最初移除 seccomp 暴露探针 `ptrace(0,...)` 会执行 PTRACE_TRACEME、改变被测进程状态并导致超时。已改用无效 request/FD/命令并清零其他参数，socketpair 在无过滤对照成功时关闭 FD；恢复探针后重复验证通过。这个修正只让固定实验无副作用，不是生产隔离策略变更。

外层 seccomp 分别强制 Landlock ABI 查询 ENOSYS、seccomp 安装 EPERM，原探针均 exit 2 / status unsupported、无 payload checks；证明确实失败关闭，无宿主 fallback。继承 fd 的反例仍如实保留为 #41 约束。

本地 `CGO_ENABLED=0 go test ./... -count=1`、build/vet、Windows build/vet 全通过；探针修正后又执行严格实验、build/vet 与 Windows build。跨平台编译不代表提供了其他平台沙箱。

## #41 实施约束与残余风险

- shim 锁定 OS 线程，安装 no_new_privs、Landlock 与 seccomp 后直接 exec，保证真正执行的 Python 及其后代继承策略；Go 多线程和 seccomp arch/x32 检查不能忽略。
- Python 解释器、stdlib、venv、动态链接器闭包需只读 allowlist。spike 为启动解释器允许的系统目录不是最终最小策略；生产不能放行工作区或 home，ExtraReadOnly 必须先获策略/审批。
- Landlock 不撤销已有 fd 的权限。独立实验预先打开宿主 sentinel 后施加限制仍可 read。#41 必须清理继承文件/联网 fd，仅留下协议需要的 stdio，清理环境凭据，并验证 symlink/路径替换、信号/ptrace 等进程间边界。
- deny socket/socketpair 和网络相关 syscall 不能容许 io_uring 绕过；过滤必须校验架构和 x32 ABI。内核隔离机制并不承诺消除内核漏洞风险。
- `RLIMIT_AS` 是虚拟内存上限；RSS 契约必须另行实现和记录（cgroup 委派或进程组采样的精度/超限窗口），不能将 #38 的 1 GiB 直接称作 RSS 配额。NPROC 按 UID 计数，不等于本次运行进程树限制。
- #41 还需 CPU/墙钟、进程组清理、父进程退出、临时磁盘和输出总量/数量预算、并发运行总预算及异常分类。#42 负责产物类型/大小/目录逃逸与清理。spike 不宣告这些能力已交付。
- 能力不足、依赖缺失或策略安装失败返回 `sandbox_unavailable`；Python 缺失返回 `runtime_unavailable`。两者均不得在宿主直接执行用户代码，也不自动切换较弱隔离。

## 官方依据

- [Landlock 内核文档](https://docs.kernel.org/userspace-api/landlock.html)：按权限/ABI 定义限制；现代 ABI 也有部分网络权限，但本项目全部 socket family 禁止目标仍由 seccomp 承担。
- [Seccomp BPF 内核文档](https://docs.kernel.org/userspace-api/seccomp_filter.html)：过滤 syscall、检查 ABI，exec/fork 后继承；单独的 syscall 过滤不是完整沙箱。
- [bubblewrap 官方说明](https://github.com/containers/bubblewrap)：隔离范围取决于实际启动参数和命名空间/挂载配置。
