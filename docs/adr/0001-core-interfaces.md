# ADR 0001：核心接口（Sandbox、VectorStore、Embedder、Approver、事件流）

## 状态

已接受（2026-10-21）。对应 issue #5。依据是技术设计 v0.3 第 5、7、9、15 节，以及测试计划里能够在接口层落地的契约（参数快照哈希、产物白名单、向量余弦、嵌入模型不一致）。

## 背景

W1 要先冻结 Core 和桌面端、评测 CLI 之间的边界。后面的平台沙箱、SQLite 向量库、Ollama / OpenAI Embedder、确认弹窗都可以替换实现，但不能改这些调用方已经依赖的签名和错误码。

约束仍然是：纯 Go，`CGO_ENABLED=0`，`core` 不 import `github.com/egoist/mygo`。运行时不可用、隔离机制不可用、嵌入模型不一致，这三件事都不得静默降级。

## 决策

接口放在现有包里，稳定错误码单独放在 `core/errcode`。每个接口都有进程内的 fake，测试不需要真实沙箱、向量库或弹窗。

| 接口 | 包 | fake |
|---|---|---|
| `Sandbox` | `core/sandbox` | `sandbox.Fake` |
| `VectorStore` | `core/rag` | `rag.FakeStore` |
| `Embedder` | `core/rag` | `rag.FakeEmbedder` |
| `Approver` | `core/approval` | `approval.Fake` |
| `Publisher` | `core/events` | `events.Recorder`；`events.Stream` 是 channel 实现 |

### Sandbox

```go
type Sandbox interface {
    Run(ctx context.Context, spec Spec) (Result, error)
}
```

`Spec` 包含源码、只读输入、资源上限、额外只读目录和 Python 运行时路径。没有网络开关。MVP 不能通过确认把网络放开，所以接口里不留这个字段。

`Run` 成功时 `error` 为 nil，`Result.ErrKind` 为空。执行失败时 `error` 是 `*ExecError`，`Result.ErrKind` 与 `Code()` 相同。上下文在开始前已经结束，记为 `timeout`。

产物只收 png、csv、json。文件名必须是输出目录里的基名，扩展名和类型一致，按文件头或语法校验，单文件硬上限 5 MiB。规则在 `ValidateArtifact`，平台实现收集文件时调用它。Fake 不碰文件系统，因此不替调用方过滤脚本里预设的产物。

### VectorStore 和 Embedder

```go
type Embedder interface {
    ID() string
    Dim() int
    Embed(ctx context.Context, texts []string) ([][]float32, error)
}

type VectorStore interface {
    Upsert(ctx context.Context, chunks []Chunk) error
    Delete(ctx context.Context, docID string) error
    Search(ctx context.Context, vec []float32, k int) ([]Hit, error)
    LastSearchStats() SearchStats
}
```

`VectorStore` 的实例绑定一个工作区，方法上不再传 workspace。PRD 里的 `Search(workspace, vector, k)` 是更早的写法；技术设计第 9 节改成「打开哪个库就是哪个工作区」。物理隔离靠打开不同文件，不靠查完再过滤。不传工作区 ID 的构造直接失败。

`LastSearchStats` 不在第 9 节的三方法列表里，但同一节要求记下检索耗时和参与的 chunk 数。把它放在接口上，是为了不把统计塞进 `Search` 的返回值。失败的检索不更新统计。

入库时默认做 L2 归一化，检索用点积，点积即余弦，用小顶堆取 top-k。分数相同则 chunk id 字典序较小的在前。维度不是常量：1024 只是 bge-m3 的当前维度。向量长度和索引维度不同，返回 `ErrDimension`，这不是 `embedding_mismatch`。

打开库时比对当前 Embedder 和 `index_meta` 的 `embedder_id` 加 `dim`。不一致时库仍然能打开，但 `Upsert` 和 `Search` 返回 `ErrEmbedderMismatch`，`Search` 不返回任何命中，`Upsert` 整批不写。`errors.Is(err, ErrEmbedderMismatch)` 为真，`Code()` 返回 `embedding_mismatch`。`Delete` 仍然允许，这样清理旧文档不会写入另一套模型的向量。

重建索引、影子表 `vectors_new`、启动时删掉残留影子表，属于以后的 SQLite 实现。契约是：重建中途失败不得把新旧向量混在同一次检索里。本次的 fake 不持久化，没有影子表。

### Approver

```go
type Approver interface {
    Request(ctx context.Context, ticket Ticket) (Decision, error)
}
```

确认单先落库，再调用 `Request`。决定只有三种：批准、拒绝、暂缓。Escape 是暂缓，返回 `Decision{Action: Defer}` 且 `error` 为 nil，运行保持 `awaiting_approval`。不响应就是不返回，直到调用方取消 `ctx`。确认超时后把运行标成 `expired` 是状态机的事，不复用沙箱的 `timeout` 码。

参数快照是规范化 JSON 的 SHA-256，小写十六进制。规范化会去掉空白并递归排序对象的键，数组顺序保留，数字保留原来的写法。同一对象（含嵌套）里的重复键直接拒绝，不取最后一个值。批准若带了另一份非空 `Params`，执行前必须重新哈希；不一致则拒绝执行。空切片与 nil 一样，表示沿用原快照。Fake 可以用「批准 + 不同参数」表示篡改，它自己不执行工具。重复决定和相反决定的幂等也不在这个接口里。

### 事件流

```go
type Publisher interface {
    Publish(ctx context.Context, ev Event) error
}
```

事件类型只有 `token`、`step`、`approval_request`、`done`。每种类型只带自己的载荷。`Stream` 把事件送进 Go channel：内部只有一个泵负责发送和关闭，所以 `Publish` 和 `Close` 可以并发。缓冲满时 `Publish` 阻塞。`Publish` 返回 nil 表示事件已经进入缓冲或已经被取走。

确认请求只带确认单 ID、动作和参数哈希。参数正文留在已经落库的确认单里，事件流不再复制一份。

`Recorder` 是内存 fake，给测试和评测轨迹使用。Core 不使用 mygo 的 `Channel`。

### 错误码

| 码值 | Go | 含义 |
|---|---|---|
| `timeout` | `errcode.Timeout` | 墙钟超时，或执行开始前上下文已结束 |
| `oom` | `errcode.OOM` | 内存超限，含父进程轮询 RSS 后杀掉 |
| `killed` | `errcode.Killed` | 被信号杀死，且不是 timeout / oom |
| `error` | `errcode.Exec` | 沙箱内程序失败，或其他未归类的执行失败 |
| `runtime_unavailable` | `errcode.RuntimeUnavailable` | Python 缺失或版本不符，不得改走无沙箱执行 |
| `sandbox_unavailable` | `errcode.SandboxUnavailable` | 隔离机制不可用，不得换一套更弱的隔离 |
| `embedding_mismatch` | `errcode.EmbeddingMismatch` | 索引与当前 Embedder 的 ID 或维度不同 |
| `stream_truncated` | `errcode.StreamTruncated` | 模型流在 `[DONE]` 和 `finish_reason` 都没到时断开，可重试；截断的正文和 tool_call 参数不能当成成功结果 |
| `path_escape` | `errcode.PathEscape` | 路径上有符号链接，或解析后不在调用方给出的根目录内；不得顺着链接删改外面的文件 |

`error` 来自技术设计第 7 节，和上面几个码一起返回给 Agent。平台暂时分不清 OOM 时，可以先返回 `killed` 或 `error`，但码值本身不能省掉；具体映射等 W3 / W4 实测后再写进评测规则。

`stream_truncated` 不复用沙箱的 `timeout`。流已经正常结束（有 `[DONE]` 或 `finish_reason`）但拼好的 tool_call 缺少 id 或 name 时，不使用这个码。

带码错误实现 `errcode.Coded`。调用方用 `errors.Is` 匹配哨兵，或用 `errcode.CodeOf` 取码。不要解析 `Error()` 的文案。

`sandbox.ErrUnsupported` 留给还没有实现的平台（MVP 的 Windows）。它不是上表里的码。

## 后果

- 桌面端和 `lantern-eval` 只依赖这些接口。替换平台沙箱、SQLite 向量库或真实 Embedder 时，不改调用方。
- 评测和单测注入 fake，即可覆盖批准、拒绝、暂缓、不响应、篡改参数，以及全部执行错误码。
- 错误码和事件类型的字符串是对外协议。改它们算破坏性变更。
- `FakeStore` 的 top-k 就是以后默认实现要保持的检索语义。SQLite 版负责持久化和影子表，不另发明一套排序。

## 不在本次范围

- darwin / linux / windows 沙箱、shim、`sandbox-exec`、Landlock、seccomp
- `kb.db`、`index_meta` 的 SQLite 迁移、影子表替换
- Ollama 和 OpenAI Embedder 的 HTTP 客户端
- 确认状态机、落库、幂等和过期
- 切分器
- 任何 mygo 类型
