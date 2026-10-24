# ADR 0010：真实 macOS 沙箱可行性与签名调用实验

## 状态与边界

已形成原生实验结论（#52/PR53），完整八阶段基线已执行通过；交付出口以独立变异、最终 CI 和合并验收为准。基线是完成 W3 验收的 main `2e84228f8f133f80c91347d52facb58bfecd5772`。本轮交付固定、有界探针、报告和后续实现决策，生产 Darwin Runner 与 SQL worker 保持失败关闭，不能用实验可运行代替后端交付。

现有 `probe-sandbox-exec.sh` 以 allow-default 能执行 true 判定工具可用；curl 失败可能只是 DNS 或无服务，不能证明网络策略。本轮使用真实受控端点和宿主成功正控。旧 Darwin 20 条 Mock 评测与 Linux 真实套件保持独立。

## 候选机制与验证问题

候选 `sandbox-exec`/SBPL 已弃用，Apple DTS 指出自定义策略缺乏第三方支持文档；不能作为跨系统版本稳定接口承诺。只按实际 OS/build/架构做结论，固定 CI 标签为 macos-15，并保存实际 runner 环境，不能由标签推断 Intel/Apple Silicon 双架构已支持。

文件与网络先有同 UID 宿主可读写 sentinel、固定 SQLite fixture 及可连接 TCP/UDP/Unix 端点，再由同一有界 payload 验证策略；合法输入、out/tmp 与依赖读取须同时成立。运行时使用显式可信准备与只读 prepared lease，不在执行路径修复依赖或降级宿主运行。Mach-O、框架与动态库依赖不能复用 Linux ELF 闭包的假设。

资源实验分开累计 CPU、Wall、RSS 与虚拟地址、输出及文件。libproc 通过现有固定版本 purego 调用以保持 CGO_ENABLED=0；采样记录实际峰值、间隔和终止时序，不宣称瞬时 RSS 硬配额。NPROC 的 UID 范围不能冒充单任务配额。

Darwin 没有已验证等价于 Linux Pdeathsig 的本项目路径。kqueue/pipe 只能通知存活的观察者，应用死亡、guardian 意外死亡和 payload 逃离进程组必须分别实验；救援只能针对创建且身份可确认的 helper，不能将事后救援包装成候选机制自行回收。

ad-hoc 签名实验使用最小 Go .app，保存签名校验及 entitlement 状态，并从实际签名 bundle 路径重跑正负例。签名通过不代表 Gatekeeper、首次安装、Developer ID 或 dmg 分发验收。

## 报告与退出语义

`lantern.darwin-spike.v1` 报告把实验完整性与候选可行性分开。默认成功只表示预定实验实际执行、正控和断言证据完整，不等同生产机制可行；真实机制不足可记录 unresolved，并给复现与后续约束。平台/运行时/策略前提不可用必须结构化失败，未执行不能计作隔离通过。安全断言被实际突破是实验失败，不能改为“可行性不足”掩盖。

报告保留实际 OS/build/arch、Go/Python、锁文件、提交/dirty、策略和 payload 指纹，分阶段记录环境、隔离、资源、生命周期与签名证据。`--require-feasible` 额外要求候选可行，供后续生产门槛使用。结果须来自实际执行，未运行阶段不得预填通过。

## 出口与后续

最终源码的真实 macOS CI、独立正负例/变异、产物审查、三角色验收、合并/main 与 lantern-notes 都完成后收口 #52。W4 仍需生产 Darwin 资源/生命周期和 SQL worker、真实 Runner/工具链、版本化 macOS 真实评测与双平台出口，不能随本 spike 关闭。

## 一手资料

- [Apple DTS：自定义 Sandbox 策略的支持边界](https://developer.apple.com/forums/thread/661939)
- [Apple：run-path dependent libraries](https://developer.apple.com/library/archive/documentation/DeveloperTools/Conceptual/DynamicLibraries/100-Articles/RunpathDependentLibraries.html)
- [Apple 开源 libproc 接口](https://github.com/apple-oss-distributions/xnu/blob/main/libsyscall/wrappers/libproc/libproc.h)
- [Apple kqueue](https://developer.apple.com/library/archive/documentation/System/Conceptual/ManPages_iPhoneOS/man2/kqueue.2.html)
- [Apple setrlimit](https://developer.apple.com/library/archive/documentation/System/Conceptual/ManPages_iPhoneOS/man2/setrlimit.2.html)
- [Apple Code Signing In Depth](https://developer.apple.com/library/archive/technotes/tn2206/)
- [GitHub 当前 runner 标签与架构](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)

## 原生启动证据与候选演变（完整验收待完成）

真实 macOS 15.7.9/build24G830、arm64、Python3.12.10 的首轮前检中，宿主固定正例成功，而隔离 payload 未进入即 SIGABRT，后七阶段未执行。静态 Mach-O 只遍历动态库会漏掉 Framework Python 包装器到 Resources/Python.app 的 SETEXEC 交接；官方 CPython 源码与宿主实际镜像链均证实该链，但补授权后仍在 dyld ignition CacheFinder 启动路径中失败，不能把该缺项写成已证唯一根因。

有界文件系统跟踪观察到宿主成功打开一个 pathname 未显示的目录 FD，继而相对定位 Cryptex；沙箱在相应初次 open 返回 EPERM 后退出。精确 /usr/lib 与 Cryptex 根目录 candidate 未解决启动问题。固定十一项单目录诊断均从相同基线独立增加 literal 权限，不自动累加或改变基线状态；其中仅增加 literal / 的试验实际进入 Python 并完成 256 MiB untouched mmap，Wait/清理成立，其余十项失败。这个证据只证明该额外权限在旧基线上有效，不证明其它授权必要或唯一根因。

下一候选手动冻结为已确认存在的 / 与 Cryptex 根目录精确 file-read-data，并移除未证必要的 /usr/lib 目录权限。OpenDirectories 与子树读取 Directories 分离：新增规则可能允许枚举这些目录的名称，本身不允许子内容/写/网络；整体另有精确运行依赖读取。该精确组合当时仍待八阶段验证，后续 4d5f3df 已完整执行通过；最终提交及七项独立原生变异仍受合并门槛约束，诊断成功不能替代完整验收。

采样口径另已校正：proc_taskinfo 的累计时间依据实际 mach_timebase_info 换算，保存 numer/denom；虚拟内存正例使用 Python 实际 mmap 长度与不触页观测，不用 Go 启动器的大 PeakVirtual 作证明。payload-ready 后采样与整条启动链资源值分列。延迟清理用具名返回值带回，实际 Lstat 不存在与自建进程 Wait/退出分开留证，不能只凭 RemoveAll 返回 nil。

## 实验结论与生产决策

在上述记录的 macOS 15.7.9/arm64/Python3.12.10 上，4d5f3df 候选的八阶段与 65 项检查完整通过，原生独立变异和最终 CI 仍是合并门槛。该结果只覆盖固定探针和所记录系统版本，生产 Darwin Runner/SQL worker 仍未实现。证据与最终提交见 PR53 和 lantern-notes/W4-01，不能用旧 Mock 或交叉编译代替。

- 最终目录候选为精确 / 与 Cryptex 根目录打开权限，已移除 /usr/lib 目录授权。实际 payload 进入后，原有文件/真实 SQLite/只读 input/可写 out/tmp、symlink/相邻前缀与网络正负例保持有效；这支持候选在该环境运行，不证明唯一根因或跨版本有效。
- fork/posix_spawn/跨进程信号在沙箱内为实际 EPERM，宿主正控成功；线程、setsid 与实际 setpgid 变组仍可成功。不能写成 Linux 的严格单线程 profile，也不能依赖进程组作为唯一身份边界。
- NPROC 实测在自有子进程由 soft/hard=1333/2000 请求仅降 soft 到零后，实际成为 0/1333；有限 fork 返回 EAGAIN35，安全恢复成为 1333/1333，并明确 exactly_original=false。独立父进程仍为 1333/2000。此 UID 计数范围及 hard 钳制行为不能归因 Seatbelt，也不能冒充单 Run 进程配额。
- 应用被杀时，guardian 通过 EOF 取消、Wait Python、清理目录后自身消失；guardian 被杀时 Python 确实存活，需要 harness 在核对 starttime/镜像后救援。两者原始结果与救援分别保存。故 feasibility=unresolved，不能直接交付声称 guardian 意外死亡也保证回收的生产后端。下一阶段须先冻结故障模型和监督设计。
- libproc CPU 使用实际 timebase，RSS、文件目录体积为采样口径。它们不是瞬时硬配额；目录扫描不能保证统计已 unlink 但仍打开的文件，单文件 RLIMIT_FSIZE 也不是整次磁盘配额。该限制属于后续生产设计问题，不以固定 payload 通过遮蔽。
- ad-hoc .app 校验成功并在实际 bundle/helper 路径重跑 Python 出图与文件/网络拒绝。签名标志为 ad-hoc；本实验未验证 Hardened Runtime、App Sandbox、Gatekeeper 或干净机器安装，不等于 W8 分发完成。

首轮独立原生测试曾被 Git archive 的标准 PAX global header 阻断：这是测试脚手架错误，不是防护变异检出。修复仅忽略元数据头，不创建该路径，其余绝对路径/穿越/符号链接/硬链接仍拒绝；以实际变异执行结果作为最终证据。
