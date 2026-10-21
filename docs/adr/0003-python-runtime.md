# ADR 0003：Python 运行时（本机检测 + 专用 venv）

## 状态

已接受（2026-10-21）。对应 issue #9。依据技术设计 v0.3 第 8 节，以及 PRD §9.3 已确认的方案：检测本机已安装的 Python，为 Lantern 创建专用 venv。内置解释器和按需下载运行时都不采用。

## 背景

沙箱里要跑 pandas 和 matplotlib。解释器如果随应用分发，体积和签名面都变大；如果临时从网络拉一份运行时，桌面端就多一条供应链。已确认的做法是使用用户机器上的 Python，再在应用数据目录里做一套只属于 Lantern 的 venv。依赖必须锁版本、锁哈希，避免每次安装解析出不同的传递依赖。

Python 缺失或版本不够时，调用方要拿到稳定错误码 `runtime_unavailable`（`core/errcode` 里已有，不新造码值），并且不能改成不经沙箱直接执行。

另外要知道 GitHub 托管的 macOS runner 上 `sandbox-exec` 还能不能用。这只决定以后 macOS 沙箱评测放在 CI 还是改到本机跑，不在这次实现 Seatbelt。

## 决策

实现放在 `core/pyrt`。纯 Go，只启动本机的 `python3` / `pip`，不把 Python 包引进 `go.mod`。

### 检测

`Config.Detect` 的顺序：

1. `Config.Python`：设置里指定的解释器。路径非空时只用它，版本不符也不改试 PATH 里的另一个。
2. PATH 上的 `python3`。
3. `DefaultSearchPaths`：macOS 依次是 `/opt/homebrew/bin/python3`、`/usr/local/bin/python3`、`/usr/bin/python3`；Linux 是 `/usr/local/bin/python3`、`/usr/bin/python3`。不查找名为 `python` 的可执行文件，避免碰到 Python 2。

版本用 `python -I -c` 读取 `sys.version_info`、`sys.executable`、`sys.base_prefix` 和标准库路径。门槛是 **≥ 3.10**。3.9 及更低返回 `runtime_unavailable`。

### 锁定的依赖

`Ensure` 在 `<应用数据目录>/pyenv/venv` 执行 `python -I -m venv`，再用 venv 里的解释器执行：

```text
python -I -m pip install --disable-pip-version-check --no-input --require-hashes --only-binary=:all: -r requirements.txt
```

锁文件是 `core/pyrt/requirements.lock`，用 `go:embed` 打进二进制，安装前写到 `<应用数据目录>/pyenv/requirements.txt`。每个分发包都是 `==` 精确版本，并且带上 PyPI 上该版本全部 wheel 的 sha256。不收录 sdist，安装时禁止从源码构建。

2026-10-21 选定的版本：

| 包 | 版本 | 原因 |
|---|---|---|
| pandas | 2.3.3 | 当时最新的 3.0.6 要求 Python ≥ 3.11，盖不住 3.10 |
| matplotlib | 3.10.9 | 3.10 系列仍要求 Python ≥ 3.10；3.11.x 要求 ≥ 3.11 |
| numpy | 2.2.6 | 仍提供 cp310–cp313 wheel。2.5.x 要求 Python ≥ 3.12，且没有和 3.10 共用的版本 |
| 其余传递依赖 | 见锁文件 | contourpy 1.3.2、pillow 12.3.0、fonttools 4.65.0、kiwisolver 1.5.1，以及 cycler、packaging、pyparsing、python-dateutil、pytz、six、tzdata |

锁文件头里的 `lantern-python: >=3.10,<3.14` 是安装范围。numpy 2.2.6 没有 cp314 wheel，所以 **3.14 及以上检测得到解释器，但 Ensure 直接返回 `runtime_unavailable`，不联网安装**。把最低版本抬到 3.11 以便改用 pandas 3，要另开一次决策，这次不改 PRD 里的 ≥ 3.10。

venv 根目录下有 `.lantern-requirements.sha256`。标记同时写下锁文件哈希、基础解释器绝对路径和版本。三者一致，且 venv 解释器版本仍在锁定范围内时，`Ensure` 不再调用 pip。标记缺失、哈希变化或基础解释器变了，就在 `<Root>/pyenv` 里新建临时目录，对这个空目录执行 `python -I -m venv`（不带 `--clear`），再 `rename` 成 `venv`。旧目录先改名挪开，删除前从解析后的 Root 再往下 Lstat：Root 以下任一层是符号链接，或解析后不在这个 Root 内，就返回 `path_escape`，不对它 `RemoveAll`。`--clear` 在沙箱外执行，目标若是指向 Root 外的符号链接，删掉的是外面的文件，所以不再使用。

`Ensure` 一开始会清掉 `pyenv` 下残留的 `.venv-new-*` 和 `.venv-old-*`。这是两次 `rename` 中间进程崩溃时留下的临时目录。只删除解析后仍在真实 Root 内的真实目录。同名的符号链接和普通文件留在原地，不跟随链接，也不对它们 `RemoveAll`。`TestEnsureSweepsRenameCrashLeftovers` 在 Root 里放好这两级目录（以及指向 Root 外的同名符号链接）再调用 `Ensure`。嵌入锁文件的哈希变了也会重建，和基础解释器变化对称，由 `TestEnsureRebuildsWhenLockHashChanges` 覆盖。

调用方给出的 `Root` 先做一次 `EvalSymlinks`，得到的真实目录是信任起点。macOS 上 `/var`、`/tmp` 本身就是符号链接，`$TMPDIR` 会经过它们；这些祖先目录不算 `path_escape`。只对真实 Root 以下的 `pyenv`、`venv`、临时目录和备份目录逐级 `Lstat`，发现符号链接就拒绝。解析之后的路径必须仍在这个真实 Root 内。缺的目录逐级 `Mkdir`，不用 `MkdirAll`。

两次路径检查之间的替换由测试钩子覆盖。`MkdirTemp`、`rename` 和删除旧目录之间仍有一小段窗口：删除前的 `O_NOFOLLOW|O_DIRECTORY` 只证明打开那一瞬间最后一层不是符号链接，fd 随即关掉，后面的 `RemoveAll` 仍按路径走。见下面「目录句柄」一节。

调用 pip 时去掉 `PIP_TARGET`、`PIP_PREFIX`、`PIP_USER`，避免包装到 venv 外面。`index-url` 等镜像配置保留。pip 退出码为 0 之后，还要在 venv 里执行 `python -I -c "import pandas, matplotlib"`（`MPLBACKEND=Agg`）。import 失败不写标记，错误码仍是 `runtime_unavailable`。

同一个 `<Root>` 上不要并发调用 `Ensure`。Unix 上用真实 Root 里的 `.pyrt.lock` 做非阻塞 `flock` 把这件事收成失败：拿不到锁就返回「已有一次 Ensure 在进行」，不清理临时目录，也不 `rename`。进程退出（包括中途崩溃）后锁会放开。非 Unix 没有 `flock`，`Ensure` 直接失败，避免无锁继续。这把锁不管符号链接，符号链接检查照旧。`TestEnsureBusyDoesNotSweep` 在锁被占着时再调用 `Ensure`，残留目录和 Root 外的文件都还在。

### 目录句柄与 TOCTOU

2026-10-21 评估了能不能用目录句柄（openat 一类）在纯 Go、不开 cgo 的前提下关掉上面的窗口。结论是盖不住，Ensure 不改成目录句柄，继续靠「同一 Root 不并发 Ensure」，并用上一节的文件锁让第二次调用失败而不是交错执行。

纯 Go 发得出 openat。Go 1.24 起的 `os.Root` 在 Unix 上就是拿着目录 fd 做 `openat` / `renameat` / `unlinkat`，Windows 上是句柄加上按 ID 打开；实现在标准库里，不走 cgo。`golang.org/x/sys/unix.Openat` 也是直接系统调用。本仓库的 Go 版本是 1.27，这条 API 用得上。

它盖不住 Ensure 要关的窗口，原因有三条：

1. 子进程只收路径。`python -m venv`、`pip install` 和 `import pandas, matplotlib` 都是 `exec` 出去的，参数是路径字符串。CPython 3.12 的 `venv.EnvBuilder.ensure_directories` 对这个路径做 `os.path.abspath` 和 `os.makedirs`，再走一遍名字查找，不会接收目录 fd。Linux 可以把 fd 放进 `ExtraFiles`，再把 `/proc/self/fd/N` 交给解释器；macOS 没有 procfs，`/dev/fd` 也不能让 venv 稳定地对着父进程已经打开的目录创建环境。这不是 cgo 能补上的：cgo 同样没法让 CPython 改走我们的 fd。解释器一旦按路径建 venv、写 `pyvenv.cfg`，中间目录被换成指向 Root 外的符号链接，写出去的仍是外面的文件。

2. `os.Root` 会跟随仍落在根里面的相对符号链接，只拒绝绝对链接和逃出根的链接。本包更严：真实 Root 以下任何符号链接都返回 `path_escape`，而不是跟着走。`os.Root` 的 openat 循环遇到链接会解析后继续（标准库内部的 `errSymlink`）。要「遇到链接就失败」，得自己用 `O_NOFOLLOW` 写遍历；`os.Root` 不把目录 fd 暴露出来。这套遍历用 `golang.org/x/sys/unix` 写得出来，也不开 cgo，但写完仍然解决不了第 1 点，Windows 还要再做一套。

3. 钉住信任根之前，打开本身还是按路径。`os.OpenRoot` 会跟随路径上的符号链接。macOS 的 `/var`、`/tmp` 必须先 `EvalSymlinks` 再打开，否则合法的 Root 又会被判成 `path_escape`。从解析完成到 `open` 返回，中间仍有一次按名查找。

所以删除、改名、创建临时目录维持现在的按路径检查。文件锁只互斥 Ensure，不拿来代替这些检查。

### 错误码

运行时失败的类型是 `*pyrt.UnavailableError`。`Code()` 返回 `errcode.RuntimeUnavailable`（字符串 `runtime_unavailable`）。`errors.Is(err, pyrt.ErrUnavailable)` 为真。调用方用 `errcode.CodeOf` 或 `errors.Is` 判断，不要解析 `Error()` 的文案。

下面这些情况用 `runtime_unavailable`：找不到解释器、版本低于 3.10、版本超出锁文件范围、解释器无法运行、`venv` 创建失败、哈希安装失败、装完后 import pandas 或 matplotlib 失败。

路径逃逸是另一类错误：`*pyrt.PathError`，`Code()` 返回 `errcode.PathEscape`（字符串 `path_escape`）。`errors.Is(err, pyrt.ErrPathEscape)` 为真。它不是 `runtime_unavailable`。解析 Root 之后，其下的 pyenv、venv 或临时目录是符号链接，或解析后跑到这个 Root 外面，都用这个码，并且不删改 Root 外的文件。Root 的祖先是符号链接时不用这个码。

下面这些不带上述两个码：`ctx` 已取消或超时（原样返回上下文错误）、`Root` 为空、嵌入的锁文件本身不合法（`ErrLockfile`，属于构建产物损坏）、同一 Root 上一次 Ensure 还拿着 `.pyrt.lock`、非 Unix 上因为没有 `flock` 而拒绝执行 Ensure。

本包不执行用户脚本，也没有「安装失败就在沙箱外把脚本跑掉」的分支。沙箱实现以后若发现运行时不可用，必须把同一个码交回去，不能降级。

Debian/Ubuntu 上如果没装 `python3-venv`，`python -m venv` 会因为没有 ensurepip 而非零退出。Ensure 把这段 stderr 放进 `runtime_unavailable`，不改用系统 site-packages。

### 交给沙箱的路径

`Runtime.Python` 是 venv 里的解释器，对应 `sandbox.Runtime.Python`。`Runtime.Base.Prefix`（`sys.base_prefix`）对应 `PyRoot`，要整棵只读挂上，只挂标准库目录会漏掉解释器和 `lib-dynload`。`Runtime.Venv` 对应 `Venv`。`MPLBACKEND=Agg` 和 `MPLCONFIGDIR` 仍由沙箱在执行用户脚本时设置。本包不设置它们。

创建 venv 和 `pip install` 在沙箱外进行，需要联网。设置页触发和审计写入不在本包：调用 Ensure 的那一层负责审计。

## 本机 Linux 实测

环境：Linux x86_64，CPython 3.12.3（`main, Aug 31 2026, 10:18:26`，GCC 13.3.0），`/usr/bin/python3`。

未安装 `python3.12-venv` 时，`python3 -m venv` 失败，原文是：

```text
The virtual environment was not created successfully because ensurepip is not
available.  On Debian/Ubuntu systems, you need to install the python3-venv
package using the following command.

    apt install python3.12-venv
```

装上该包之后，`PYRT_ENSURE_REAL=1 go test ./core/pyrt/ -run TestEnsureReal` 通过，用时 11.00s（含下载 wheel，以及紧接着的第二次 Ensure）。venv 解释器在 `MPLBACKEND=Agg` 下执行 `import pandas, matplotlib`，打印 `2.3.3` 和 `3.10.9`。pip 使用锁文件里的 sha256，`--require-hashes` 没有拒绝这些 wheel。

这次 Linux 安装只证明锁文件在 CPython 3.12 / manylinux x86_64 上能装上。macOS wheel 的哈希已经写进锁文件，但没有在 macOS 上执行安装。

## macOS runner 实测

测量方法：工作流 job `macos-sandbox-exec` 使用 `macos-latest`，执行 `scripts/probe-sandbox-exec.sh`，上限 10 分钟。可用的判定是：`sandbox-exec` 存在且可执行，并且 `(version 1)(allow default)` 能让 `/usr/bin/true` 以退出码 0 结束。job 在这个判定失败时失败。

另外记录、但不单独决定 job 成败的有：无参数时的输出、一段 `(deny default)` 策略能否跑通 `/usr/bin/true`、命名 profile `no-network`，以及 `(version 1)(allow default)(deny network*)` 下 `/usr/bin/curl -fsS --max-time 8 https://example.com` 的退出码。`resources/darwin/lantern.sb` 仍是占位说明，这次不用它做探测。

来源：pull request #22 的检查，workflow run [`39569378678`](https://github.com/loveRyujin/lantern/actions/runs/39569378678)，job `macos-sandbox-exec`，2026-10-21 08:27 UTC，结论 success。同一提交的 push 运行 `39569358681` 也是 success。下面是该 job 里探测脚本的标准输出：

```text
uname: Darwin iad20-eo1205-055ec939-cd86-431a-9ee1-52e3caccd5a5-9EEAA4ACEFC4.local 25.6.0 Darwin Kernel Version 25.6.0: Fri Jul 31 19:16:43 PDT 2026; root:xnu-12377.161.14~5/RELEASE_ARM64_VMAPPLE arm64
runner_os: macOS
image_os: macos26
image_version: 20261013.0424.1
product_name: macOS
product_version: 26.6.2
build_version: 25G83
sandbox_exec: /usr/bin/sandbox-exec
-rwxr-xr-x  1 root  wheel  102368 Aug 13 02:51 /usr/bin/sandbox-exec
---- sandbox-exec 无参数 ----
exit: 64
  Usage: sandbox-exec [options] command [args]
  Options:
    -f profile-file     Read profile from file.
    -n profile-name     Use pre-defined profile.
    -p profile-string   Specify profile on the command line.
    -D key=value        Define a profile parameter.
  Exactly one of -f, -n, -p must be specified.
---- allow default /usr/bin/true ----
exit: 0
---- deny default /usr/bin/true ----
exit: 0
---- named profile no-network /usr/bin/true ----
exit: 0
---- deny network curl example.com ----
exit: 6
  curl: (6) Could not resolve host: example.com
结论: sandbox-exec 可用; allow_default=0; deny_default=0; named_no_network=0; network_deny_curl=6; bare=64
```

Runner 镜像在作业设置阶段报的是 macOS 26.6.2（25G83），镜像名 `macos-26-arm64`，版本 `20261013.0424.1`。内核是虚拟化的 Apple Silicon（`RELEASE_ARM64_VMAPPLE`）。

结论：在这台 `macos-latest` 上 `sandbox-exec` 可用。`(allow default)` 和上面那段 `(deny default)` 都能执行 `/usr/bin/true`。命名 profile `no-network` 也能执行 `/usr/bin/true`。无参数时退出码 64，输出是用法说明，这段文字里没有弃用警告。`(deny network*)` 下 curl 没有拿到 `example.com`，退出码 6，原因是无法解析主机名。这只说明这次请求没有成功，不能代替以后用逃逸用例去核对每一种网络系统调用。

因此 W4 的 macOS 沙箱评测可以留在 GitHub Actions 的 `macos-latest` 上，不必因为 runner 缺少 `sandbox-exec` 而改到本机跑。

同一个 `macos-sandbox-exec` job 在探测之后跑 `go test ./core/pyrt/`。#25 复盘时，`$TMPDIR` 经过 `/var` → `/private/var` 这类祖先符号链接，`Ensure` 被误判成 `path_escape`；当时 CI 只有 Linux 的 `go test`，所以没拦住。跟进时决定把这一步加到 macOS runner 上。它不设置 `PYRT_ENSURE_REAL`，不下载 wheel，也不代替 Linux 上的 `pyrt-install`。

## 后果

- 桌面端准备运行时只调用 `pyrt.Config.Ensure`。替换锁文件版本要改 `requirements.lock` 并更新本节的版本表。同一 Root 上第二次并发调用会因为 `.pyrt.lock` 失败，而不是和第一次交错替换。非 Unix 上 Ensure 直接失败。
- 目录句柄（`os.Root` / openat）在纯 Go、不开 cgo 下发得出，但盖不住 `python` / `pip` 按路径查找的窗口，也不能在不放宽「Root 以下任何符号链接都拒绝」的前提下替换现有检查。Ensure 因此不改成目录句柄。
- 用户机器是 Python 3.14 或更高时，现在会明确失败，而不是装上一半。
- `sandbox-exec` 的探测和 Python 安装互相独立。macOS job 不下载 pandas。2026-10-21 的 `macos-latest`（macOS 26.6.2）上探测通过，后续 macOS 沙箱评测可以挂在这个 runner 上。同一 job 会跑 `go test ./core/pyrt/`（假解释器，不联网安装）。
- 参数快照另补了一条回归：`ParamsHash("[]")` 不得等于 `ParamsHash("null")`。空数组必须保持为数组。这与运行时无关，只是同一批改动里带上的。回归测试是 `core/approval/approval_test.go` 的 `TestParamsHashEmptyArrayIsNotNull`，来源提交是 [`55ba369`](https://github.com/loveRyujin/lantern/commit/55ba369b63e3ab4e7618975d669b6cfb25437698)（完整 SHA `55ba369b63e3ab4e7618975d669b6cfb25437698`，PR #22「检测本机 Python 并准备带哈希锁定的专用 venv」）。该提交只加了这条测试，没有改 `core/approval/hash.go`；规范化本身在 #16（`d9debc9`）里已经把空数组写成 `[]`、把 JSON null 写成 `null`。#22 的提交说明里写的是「参数快照补上空数组与 null 的哈希必须不同」。

## 不在本次范围

- SBPL 策略正文、`sandbox-exec -f`、资源限制 shim
- 设置页、审计记录、在沙箱里执行用户脚本
- 把最低 Python 版本改成 3.11 并升到 pandas 3
- 在 macOS 上实际执行一次哈希安装。Linux 的 `pyrt-install` job 会跑 `PYRT_ENSURE_REAL=1`；macOS wheel 仍只锁了哈希，没有在 Mac 上装过
- `TestDetectCanceled` 依赖假解释器 `sleep 30` 和 3 秒上限。进程组杀掉之后应当马上返回；慢 runner 上若杀不掉，测试会先撞上这 3 秒
- Windows 的常见安装路径（PATH 和用户指定路径仍然有效）。`ci` job 会跑 `GOOS=windows go vet ./...`，用来在合入前看到 Windows 编不过的测试和实现；这一步不执行测试，也不在 Windows 上创建 venv
