# ADR 0010：真实 macOS 沙箱可行性与签名调用实验

## 状态与边界

实验中（#52），已取得真实 macOS 启动失败及独立目录诊断结果，尚未完成完整基线验收。基线是完成 W3 验收的 main `2e84228f8f133f80c91347d52facb58bfecd5772`。本轮交付固定、有界探针、报告和后续实现决策，生产 Darwin Runner 与 SQL worker 保持失败关闭，不能用实验可运行代替后端交付。

现有 `probe-sandbox-exec.sh` 以 allow-default 能执行 true 判定工具可用；curl 失败可能只是 DNS 或无服务，不能证明网络策略。本轮使用真实受控端点和宿主成功正控。旧 Darwin 20 条 Mock 评测与 Linux 真实套件保持独立。

## 候选机制与验证问题

候选 `sandbox-exec`/SBPL 已弃用，Apple DTS 指出自定义策略缺乏第三方支持文档；不能作为跨系统版本稳定接口承诺。只按实际 OS/build/架构做结论，固定 CI 标签为 macos-15，并保存实际 runner 环境，不能由标签推断 Intel/Apple Silicon 双架构已支持。

文件与网络先有同 UID 宿主可读写 sentinel、固定 SQLite fixture 及可连接 TCP/UDP/Unix 端点，再由同一有界 payload 验证策略；合法输入、out/tmp 与依赖读取须同时成立。运行时使用显式可信准备与只读 prepared lease，不在执行路径修复依赖或降级宿主运行。Mach-O、框架与动态库依赖不能复用 Linux ELF 闭包的假设。

资源实验分开累计 CPU、Wall、RSS 与虚拟地址、输出及文件。libproc 通过现有固定版本 purego 调用以保持 CGO_ENABLED=0；采样记录实际峰值、间隔和终止时序，不宣称瞬时 RSS 硬配额。NPROC 的 UID 范围不能冒充单任务配额。

Darwin 没有已验证等价于 Linux Pdeathsig 的本项目路径。kqueue/pipe 只能通知存活的观察者，应用死亡、guardian 意外死亡和 payload 逃离进程组必须分别实验；救援只能针对创建且身份可确认的 helper，不能将事后救援包装成候选机制自行回收。

ad-hoc 签名实验使用最小 Go .app，保存签名校验及 entitlement 状态，并从实际签名 bundle 路径重跑正负例。签名通过不代表 Gatekeeper、首次安装、Developer ID 或 dmg 分发验收。

## 报告与退出语义

`lantern.darwin-spike.v1` 报告把实验完整性与候选可行性分开。默认成功只表示预定实验实际执行、正控和断言证据完整，不等同生产机制可行；真实机制不足可记录 unresolved，并给复现与后续约束。平台/运行时/策略前提不可用必须结构化失败，未执行不能计作隔离通过。安全断言被实际突破是实验失败，不能改为“可行性不足”掩盖。

报告保留实际 OS/build/arch、Go/Python、锁文件、提交/dirty、策略和 payload 指纹，分阶段记录环境、隔离、资源、生命周期与签名证据。`--require-feasible` 额外要求候选可行，供后续生产门槛使用。当前尚无本轮结果，不能预填通过。

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

下一候选手动冻结为已确认存在的 / 与 Cryptex 根目录精确 file-read-data，并移除未证必要的 /usr/lib 目录权限。OpenDirectories 与子树读取 Directories 分离：新增规则可能允许枚举这些目录的名称，本身不允许子内容/写/网络；整体另有精确运行依赖读取。该精确组合仍须重新执行八阶段、签名后的正负例和七项独立原生变异，诊断成功不能替代完整验收。

采样口径另已校正：proc_taskinfo 的累计时间依据实际 mach_timebase_info 换算，保存 numer/denom；虚拟内存正例使用 Python 实际 mmap 长度与不触页观测，不用 Go 启动器的大 PeakVirtual 作证明。payload-ready 后采样与整条启动链资源值分列。延迟清理用具名返回值带回，实际 Lstat 不存在与自建进程 Wait/退出分开留证，不能只凭 RemoveAll 返回 nil。
