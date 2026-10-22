# lantern 技术设计 v0.3

依据：PRD v0.3（[`../prd/PRD-v0.3.md`](../prd/PRD-v0.3.md)，含文末 13:36 已确认决策）、mygo 调研笔记。取代 v0.2（服务端 + Postgres 方案作废）。状态：草案，待评审。

## 1. 总体约束
- 纯 Go，`CGO_ENABLED=0`；Go ≥ 1.27（mygo 要求）；`go.mod` 锁定 mygo v0.6.0 的具体 commit，8 周内不主动升级。
- 分层：`core`（不 import mygo）→ 两个入口：`cmd/lantern`（mygo 桌面端）、`cmd/lantern-eval`（headless CLI，CI 用）。
- 唯一出网：grok4.6 API；Ollama 走 localhost。

## 2. 关键选型

| 项 | 选择 | 理由 |
|---|---|---|
| SQLite 驱动 | **`modernc.org/sqlite`（建议）** | 纯 Go 转译，无需运行时下载预编译库；Core、评测 CLI、桌面端用同一个驱动，CI 零额外依赖。mygo sqlite 插件靠 purego 加载预编译库、`go test` 时要下载，也不能加载扩展，对我们没有额外好处 |
| 向量存储 | `VectorStore` 接口；默认 `sqlitevec`：float32 BLOB（入库时 L2 归一化）+ 启动时整库载入内存 + 点积 top-k（小顶堆） | 作品集规模够用；W5 做基准测试 |
| Embedding | Ollama bge-m3，`/api/embed` 批量调用，1024 维 | 已定 |
| LLM | grok4.6，自写 OpenAI 兼容的薄客户端（tool_calls + SSE 流式） | Agent 循环自己写 |
| JSON Schema 校验 | `santhosh-tekuri/jsonschema/v6`（纯 Go） | |
| SQL 校验 | 自写轻量词法器（处理注释、字符串、大小写），只接受单条 `SELECT`/`WITH`；再加上只读连接、`PRAGMA query_only=ON`、authorizer 白名单兜底（见 §6） | pg_query_go 需要 cgo，不能用 |
| UI | mygo Native UI，`internal/ui` 薄封装 | 已定 |
| 日志 | `log/slog` JSON，写到应用数据目录 | |

## 3. 仓库结构
```
lantern/
├─ cmd/
│  ├─ lantern/          # mygo 桌面端入口
│  ├─ lantern-eval/     # headless 评测
│  └─ lantern-seed/     # 合成数据 + 工作区生成
├─ core/
│  ├─ agent/            # 循环、消息、步骤控制
│  ├─ llm/              # Provider：grok、mock
│  ├─ tools/            # registry + 五个工具
│  ├─ policy/
│  ├─ run/              # 状态机、run_steps、恢复
│  ├─ approval/         # Approver 接口、确认单、超时
│  ├─ rag/              # chunker、embedder、vectorstore
│  ├─ sqlx/             # 只读执行、语句校验
│  ├─ sandbox/          # 接口 + darwin/linux/windows（构建标签）+ shim
│  ├─ pyrt/             # Python 检测、venv 管理
│  ├─ workspace/        # 目录布局、打开各库
│  ├─ audit/
│  └─ events/           # 类型化事件流
├─ internal/ui/         # 只有这里 import mygo
├─ resources/darwin/lantern.sb   # SBPL 策略
├─ eval/v1/
├─ data/                # 口径文档、敏感字段清单
└─ Makefile             # seed / test / eval / build
```

## 4. 工作区与存储（物理隔离）
```
~/Library/Application Support/Lantern/   （Linux：$XDG_DATA_HOME/lantern）
└─ workspaces/<ws-id>/
   ├─ kb.db         # documents、chunks、vectors
   ├─ analytics.db  # 业务表，users 只有脱敏列
   ├─ sensitive.db  # phone/email 原文，只有 export_data 确认后才会打开
   ├─ runs.db       # runs、run_steps、run_events、approvals、audit
   ├─ exports/      # 确认后的导出文件
   └─ tmp/          # 沙箱临时目录（每次执行一个子目录）
```
- Core 只认 `workspace.Handle`，每个库按路径分别打开，没有“共用表 + 过滤”这种做法，跨工作区访问从结构上就做不到。
- `audit` 表只有 INSERT 路径；用 SQLite trigger 拒绝 UPDATE/DELETE，作为纵深防御（本机用户直接改文件不在防护范围内，PRD 已说明）。
- **runs.db 的信任边界**：只有 Core 能写 `runs.db`（运行、步骤、确认单、审计）。工具和沙箱都不能写这个库。`ConfirmExport` 相信库里的审批记录。现在还没有把工具进程和 `runs.db` 做成硬隔离；要不要做、怎么做，放到 W2 的审批状态机里再定。

## 5. Agent 循环与状态机
沿用 v0.2 的设计，存储换成 SQLite：
- 每一步结束后把 messages 和 step 存进 `runs.context`；工具执行前 `run_steps.status=executing`，执行完改成 `done|error`。
- 状态迁移：`UPDATE runs SET state=?, version=version+1 WHERE id=? AND state=? AND version=?`，SQLite 用 WAL 模式加单写连接。
- 重启恢复（FR-SM-5）：最后一步是 done 就继续跑；在 awaiting_approval 就保持挂起；最后一步是 executing 就标为 failed 并写审计。
- 确认恢复时用 SHA-256（规范化 JSON）重新计算参数快照的哈希，和记录比对，不一致就拒绝执行。
- `Approver` 接口：`Request(ctx, Ticket) (Decision, error)`。UI 实现会弹出 alert-dialog，按 Escape 视为暂缓；脚本实现按用例预设批准、拒绝、不响应或篡改参数。确认单先落库，再通知 Approver，所以 UI 崩了也不会丢单。
- 退出拦截：在 `OnBeforeQuit` 里检查是否有 running 的运行。
- 事件流：Core 发 `events.Event`（token、step、approval_request、done）到 Go channel；UI 层的 goroutine 消费后调用 `Window.Update`；评测 CLI 直接把事件写进轨迹。

## 6. SQL 工具（SQLite）
三层防护：
1. 词法校验：去掉注释后只允许一条语句，首个关键字必须是 `SELECT` 或 `WITH`，拒绝 `ATTACH`、`PRAGMA`、`load_extension` 等关键字和函数。
2. 连接层：用 `file:analytics.db?mode=ro` 打开，并设置 `PRAGMA query_only=ON`；`sensitive.db` 从不 ATTACH 到这个连接上。
3. authorizer 回调：如果 modernc 暴露了 `sqlite3_set_authorizer`，就只放行 SQLITE_READ 和 SQLITE_SELECT 这类操作；W2 先确认这个 API 能不能用，用不了就只靠前两层，并在报告中注明。
- **Linux 查询资源隔离（#38）**：`NewTool` 每次启动自身可执行文件的 SQL worker。shim 先设置 `RLIMIT_AS` 软/硬上限均为 **1 GiB（1,073,741,824 字节）**，禁用 core dump，再 exec worker，让 Go 运行时在上限内启动。上限覆盖整个虚拟地址空间，包括运行时预留，不能当作 1 GiB RSS/SQLite 堆配额。`GOMEMLIMIT=64MiB` 只降低 Go 堆压力，不充当硬限制。SQLite NOMEM 或明确的 Go runtime OOM 映射为 `sql_result_too_large`；分配失败后 worker 结束，不要求内核必须发 OOM kill。普通崩溃或协议损坏是 `sql_worker_failed`，启动失败是 `sql_worker_unavailable`。所有错误均不返回部分结果，资源错误仍不计入也不清零 Agent 连续参数失败计数。
- **进程生命周期与观测**：父进程核对 `ready`/`done` 的版本、PID、上限和错误码，并采集 `/proc` 的 VmPeak/VmHWM 与 wait4 的峰值 RSS；正常与异常路径均 Wait。context 到期杀整个进程组，父进程死亡时内核杀 worker。环境只传运行时参数及临时目录，不继承 API Key。stderr 和协议输出有固定长度上限。此机制只交付 Linux；其他平台返回明确不可用，macOS 隔离在 W4 实现，不能无声回退到主进程。
- **查询与结果预算**：不改写 SQL。默认 200 行、最多 1000 行，结果列在 prepare 后、step 前限制为 32 列，保留单值 `SQLITE_LIMIT_LENGTH=4 MiB`，不使用会误伤宽表的 `SQLITE_LIMIT_COLUMN`。最终 **完整 JSON 不超过 1 MiB**，包括列名、元数据、JSON 转义与 base64；预留少量截断元数据。单行超限整次返回 `sql_result_too_large`；多行累计超限保留完整行并标记字节截断。JSON 关闭 HTML 转义；合法 TEXT 保持字符串，非法 UTF-8 TEXT 以 `{encoding:"base64",data:"..."}` 对象无损表示，BLOB 保持 base64 字符串。worker 协议解码使用 `json.Number`，不丢失 int64 精度。
- **只读与文件身份**：工具构造时冻结解析后的登记路径与文件身份；查询前父进程及 worker 校验设备号/inode，worker 在查询后再次校验。文件被替换或重定向时返回 `data_busy`，调用方重新打开并构造工具。完整文件哈希改为流式读取并在 worker 内计算，父进程只观察 WAL 是否出现，避免为算哈希而关闭额外 fd、释放同进程写连接的 POSIX 锁。模型 SQL 只走 `NewTool`；公开的低层 `Query` 是当前进程读取器，本身不限制中间值总内存。
- **回归与边界**：六类真实 B6 查询（物化 CTE、子查询、GROUP BY、UNION、IN 列表、ORDER BY random()）各用独立 worker 验证错误与系统峰值，不只检查退出码。原先 1.2–2.75 GB 的中间值风险由 Linux worker 的地址空间硬限制覆盖。`temp_store=FILE` 和 256 KiB 页缓存仍只帮助排序，不替代硬限制；不使用会影响主进程审计/工作区写入的进程共享 SQLite hard heap limit。一般 SIGSEGV/SIGKILL 不会当作 OOM；只有确认限额生效且有明确 runtime OOM 诊断的异常退出才映射为结果过大。文件系统/网络级的 Python Sandbox 仍属于 W3 后续任务。

- 只读连接不加 `immutable`。导入、种子或迁移写入时拿着锁。`run_sql` 拿不到这把锁，就返回可重试的 `data_busy`，不把行交给模型。库已经是 WAL 模式、旁边却没有 `-wal` 时，只读打开会把 `-wal` 建出来；查询前后的哈希从这次连接建立之后开始算。
- 每次查询后再核对一次。主库哈希变了、已有 `-wal` 的哈希变了，或者新出现了 `-wal`，这次结果丢掉，同样返回可重试的 `data_busy`。可能过期的行不会交给模型。Open 之后才出现 `-wal` 时也是 `data_busy`，避免连接还停在打开时的快照上。`-shm` 不参与哈希：它是 WAL 的共享内存索引，只读连接为了协调也会改它，里面没有业务行。
- `PRAGMA` 和 `pragma_*` 表值函数都拒绝。双引号、方括号、反引号里的名字按解析后的标识符判断，不在整段 SQL 里搜子串。
- `SELECT sqlite_master` 允许，看到的是业务库自己的表。连接只打开工作区登记的那一个业务库（`analytics.db`）。要打开的路径和登记路径都先做绝对化和 `EvalSymlinks`，再比较，并用 `os.SameFile` 核对文件身份。指向别处的符号链接、改名副本和硬链接都打不开。`sensitive.db` 也不能靠 `ATTACH` 或 URI 参数进来。

### SQL worker 当前边界与验收

- 文件哈希与身份检查的测试必须实际到达 worker 的查询后校验：同 inode 内容改变与内容相同但 inode 改变分别验证；临时禁用对应检查后，测试必须失败。父进程的测试钩子和启动前替换测试不能代替这两项。
- 路径两侧校验尚未绑定 SQLite 实际持有的文件描述符，不能抵御具有工作区写权限的同用户进程在检查间隙来回替换路径。工作区目录仍属于应用信任边界，后续 Python 沙箱不得写入这些目录。
- Linux `Pdeathsig` 实际绑定发起创建的 OS 线程，线程退出也可能使 worker 异常退出；此时按 `sql_worker_failed` 处理，不伪装成 OOM。临时文件尚无磁盘配额，墙钟超时不是磁盘硬上限；当前 Agent 串行调用工具，尚未提供全局 worker 并发预算。它们是后续资源隔离工作需要继续覆盖的边界。

## 7. 沙箱
统一接口 `Sandbox.Run(ctx, Spec) (Result, error)`，错误类型分为 `timeout | oom | killed | error | runtime_unavailable`。

**资源限制（两个平台通用）**：纯 Go 设不了子进程的 rlimit，所以用 lantern 自己的可执行文件做一个 shim：`lantern __sandbox-shim --cpu=.. --as=.. --nproc=.. -- <cmd>`。shim 先调用 `syscall.Setrlimit`，再 `syscall.Exec`。父进程用 `Setpgid` 开新进程组，超时就 kill 整个进程组；同时轮询子进程的 RSS，作为内存限制的兜底（macOS 上 RLIMIT_AS 不可靠，待实测）。

**macOS**：`sandbox-exec -f lantern.sb -D INPUT=.. -D OUTPUT=.. -D PYROOT=.. -D VENV=.. shim ... python run.py`
- SBPL：`(deny default)`，放行 process-exec 和 fork；对 PYROOT、VENV、INPUT、系统库只读，对 OUTPUT 和 TMP 可读写，`(deny network*)`。
- 进程数用 RLIMIT_NPROC 限制，但它按用户计数，在 macOS 上需要实测；不可靠的话改为 shim 轮询子进程数。

**Linux（#40 spike，参见 [ADR 0005](../adr/0005-linux-sandbox.md)）**：
- A. bubblewrap：`bwrap --unshare-all --die-with-parent --new-session --ro-bind PYROOT --ro-bind VENV --ro-bind INPUT /in --bind OUTPUT /out --tmpfs /tmp --proc /proc --dev /dev`，再经 shim 设 rlimit。优点是网络命名空间隔离最彻底，缺点是要用户另装 bwrap，而且部分发行版限制非特权 user namespace（比如 Ubuntu 24.04 的 AppArmor 限制）。
- B. Landlock + seccomp：shim 内用 `landlock-lsm/go-landlock` 限制文件系统、用纯 Go 的 seccomp BPF 库禁掉 socket/connect 等系统调用，然后 exec。优点是零外部依赖，缺点是必须探测实际 ABI/权限；当前全部 socket family 的禁止策略由 seccomp 承担，不能仅凭内核版本判断支持。
- #40 以 **B：Landlock + seccomp 为主方案**进行真实探针验收，ABI >= 3 并实际安装策略后才可启用；A 为实验对照，不提供自动降级。开发机与 GitHub runner 的最终证据、限制见 ADR 0005。spike 不实现 `Sandbox.Run`，完整资源/生命周期与工具链仍由 #41–#44 验收。

**数据与产物**：SQL 结果写成 `tmp/<exec>/in/*.csv`（只读），产物从 `out/` 收集，只收 png/csv/json，单文件不超过 5MB，并按文件头校验真实类型；执行完删除临时目录。

## 8. Python 运行时（已决：检测本机 Python + 专用 venv）
- 检测顺序：用户在设置里指定的路径 → `python3` 在 PATH 中的位置 → 常见路径（Homebrew、`/usr/bin`）。版本要求 ≥3.10。
- 在 `<appdata>/pyenv/venv` 创建 venv，用 `pip install` 安装锁定版本的 pandas、matplotlib（requirements 带哈希）。这一步在沙箱外进行，需要联网，在设置页由用户手动触发，并写审计。
- 沙箱只给 base Python 和 venv 只读权限；`MPLBACKEND=Agg`、`MPLCONFIGDIR` 指向 tmp。
- 运行时不可用时返回 `runtime_unavailable`，**绝不降级为不经沙箱直接执行**。

## 9. RAG
- 切分：`fixed`（512 token，重叠 64；token 数先按字符数近似）和 `structural`（按标题和口径条目切）。
- `VectorStore` 接口：`Upsert(ctx, []Chunk) / Delete(ctx, docID) / Search(ctx, vec, k) ([]Hit, error)`，实例绑定到单个工作区。
- 检索时用 `[]float32` 在内存里做点积 + 小顶堆；记录检索耗时和 chunk 数（FR-RAG-8）。
- 引用校验、无依据拒答与 v0.2 相同。

## 10. 评测 CLI
- `lantern-eval run --category=... --mode=mock|live --platform=auto`，复用 Core，用脚本化 Approver。
- 沙箱用例按 `runtime.GOOS` 选择 `eval/v1/sandbox/{darwin,linux}`。
- CI：Linux runner 跑 mock 评测和 Linux 沙箱评测；macOS runner 跑 macOS 沙箱评测；RAG 和端到端（需要 Ollama 和 grok）只在本地跑，结果提交到 `reports/`。

## 11. 分发
- macOS：`mygo build` 产出 .app，ad-hoc 签名（`codesign -s -`）后打包 dmg，README 说明首次打开需要右键打开或执行 `xattr` 放行。不开 App Sandbox。W4 验证 ad-hoc 签名的应用能调用 `sandbox-exec`、能 exec 自己做 shim。
- Linux：P1 再做 tar.gz。

## 12. 待确认
1. SQLite 驱动用 modernc（我的建议，需要产品和 ryujin 认可）。
2. Linux 沙箱主方案与支持条件由 #40 / ADR 0005 记录；实际 Sandbox.Run 由 #41 验收。
3. authorizer 能否在 modernc 中使用，W2 确认。

## 13. W1 顺序
1. 仓库骨架、Core 与 UI 分层、`CGO_ENABLED=0` 的 CI 检查
2. workspace 目录布局、各库的迁移、`lantern-seed`
3. LLM Provider（grok + mock）、事件流
4. Sandbox、VectorStore、Approver 接口定义
5. `lantern-eval` 骨架
6. mygo spike（半天）：窗口 + `markdown.Stream` + alert-dialog
7. Python 检测与 venv 的 spike

## 14. v0.3.1 补丁（采纳测试搭子的意见）
- **Linux 网络**：Landlock 新 ABI 含部分网络权限；本项目仍一律用 seccomp 拒绝 `socket()`（AF_INET、AF_INET6、AF_UNIX、AF_NETLINK、RAW 全部拦截）。内核不支持 Landlock 时直接返回 `sandbox_unavailable`，不静默降级。
- **macOS 内存**：不依赖 RLIMIT_AS，改由父进程按 100ms 间隔轮询整个进程组的 RSS，超限就 kill 并返回 `oom`，报告里注明这是平台差异。
- **ATTACH**：除了词法校验，还对 analytics 连接设置 `SQLITE_LIMIT_ATTACHED=0`（如果 modernc 暴露了 limit 接口），并启用 authorizer 拒绝 SQLITE_ATTACH。另外的兜底：`run_sql` 放进 shim 子进程执行，套用和沙箱同样的文件系统策略（只能读 analytics.db），所以即使 ATTACH 成功也打不开 sensitive.db。W2 确认哪几层能用。

## 15. 可插拔 Embedder（v0.3.2，ryujin 已确认）
```go
type Embedder interface {
    ID() string          // 如 "ollama:bge-m3"、"openai:text-embedding-3-small"
    Dim() int
    Embed(ctx context.Context, texts []string) ([][]float32, error)
}
```
- 实现：`ollama`（默认，bge-m3，1024 维）、`openai`（兼容 OpenAI 接口，grok 等同样走这个实现，只换 base URL 和模型名）、`mock`（评测用，结果确定）。
- `kb.db` 新增 `index_meta(embedder_id, dim, normalize, created_at)` 表，只允许一行；每个 chunk 也记下 `embedder_id`。
- 打开工作区时比对当前配置的 Embedder 和 `index_meta`：
  - 一致：正常使用。
  - 不一致：`VectorStore.Search` 和导入都返回 `ErrEmbedderMismatch{have, want}`，Agent 收到后告诉用户“索引与当前模型不一致”；UI 提示用户重建索引。**不做任何回退，也不混用两种模型的向量。**
- 重建索引：把新向量写进影子表 `vectors_new`，全部成功后在一个事务里替换旧表并更新 `index_meta`；中途失败时旧索引保持可用。
- 维度不再写死：BLOB 长度按 `index_meta.dim` 校验，读到长度不符的向量直接报错。
- 评测报告的环境信息里加上 `embedder_id` 和 `dim`。
- 错误码固定为 `embedding_mismatch`（Go 侧 `ErrEmbedderMismatch` 的 `Code()` 返回该值）；比对按 `embedder_id`（模型名）加 `dim`，不只看维度；影子表 `vectors_new` 在启动时如果存在就删掉，保证新旧向量不混用。
