package sqlx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/loveRyujin/lantern/core/tools"
)

// Event 是一次拒绝要留给审计的内容。本包不依赖 audit，避免和审计的测试形成导入环。
type Event struct {
	Actor  string
	Action string
	RunID  string
	Detail string
}

// Logger 把 [Event] 写进审计。nil 表示不记录。
type Logger interface {
	Append(ctx context.Context, ev Event) error
}

// CodeRejected 是语句被拒绝时返回给模型的错误码。
// 参数本身不合法仍用 tools 的 tool_arguments_*，那一步在执行前由注册表完成。
const CodeRejected = "sql_rejected"

// CodeBusy 表示分析库正在被写入，或者查询期间文件发生了变化。
// 可重试。这次不返回行，避免把可能过期的结果交给模型。
const CodeBusy = "data_busy"

// CodeResultTooLarge 表示查询进程超出内存预算，或结果超过字节上限。
// 这次不返回任何行，包括这一行之前已经读出的行。不可重试。
// 这不是参数失败。循环既不把它计入连续失败，也不因此把连续失败清零。
const CodeResultTooLarge = "sql_result_too_large"

// Tool 是 run_sql。用 [tools.SpecRunSQL] 注册，不另写一套参数说明。
type Tool struct {
	db    Queryer
	path  string
	log   Logger
	after func()
	query func(context.Context, Queryer, string, int) (Result, error)
	local bool // 仅供包内低层读取器测试；产品构造器不启用。
}

// NewTool 绑定已经用 [Open] 打开的只读库。
// path 必须是打开时用的那个路径，而且和 allowed 是同一个文件。allowed 是工作区登记的业务库。
// Linux 查询在独立 worker 里执行，父进程不使用 db 执行模型 SQL。
// 构造时固定解析后的路径与文件身份；文件替换后返回 data_busy，调用方需重新打开并构造工具。
// worker 保留只读连接与查询两侧的文件哈希核对。其他平台在隔离实现前返回 sql_worker_unavailable。
// log 可以为 nil，此时拒绝语句不会写审计。
// 调用方把 [Event] 转成自己的审计记录即可，字段已经带好 actor、action 和 RunID。
func NewTool(db Queryer, path, allowed string, log Logger) (*Tool, error) {
	if db == nil {
		return nil, errors.New("sqlx: 没有数据库")
	}
	registered, err := allowBusiness(path, allowed)
	if err != nil {
		return nil, err
	}
	identity, err := os.Stat(registered)
	if err != nil {
		return nil, err
	}
	device, inode := fileIdentity(identity)
	t := &Tool{db: db, path: registered, log: log}
	t.query = func(ctx context.Context, _ Queryer, query string, limit int) (Result, error) {
		current, statErr := os.Stat(registered)
		resolved, resolveErr := Resolve(registered)
		if statErr != nil || resolveErr != nil || resolved != registered || !os.SameFile(identity, current) {
			return Result{}, busyErr()
		}
		result, _, err := runSQLProcess(ctx, processRequest{Path: registered, Allowed: registered, SQL: query, Limit: limit, Device: device, Inode: inode})
		return result, err
	}
	return t, nil
}

// Spec 实现 tools.Tool。
func (t *Tool) Spec() tools.Spec {
	return tools.SpecRunSQL
}

// Execute 先扫描语句，再在只读连接上查询，最后核对文件哈希。
// 扫描不通过时不会把语句送到连接。SQL 原样交给驱动，不用 Sprintf 拼进别的语句。
func (t *Tool) Execute(ctx context.Context, call tools.Call) (tools.Result, error) {
	if t == nil || t.db == nil {
		return tools.Result{}, errors.New("sqlx: 没有数据库")
	}
	if err := ctx.Err(); err != nil {
		return tools.Result{}, &codedTimeout{err: err}
	}
	query, limit, err := parseArgs(call.Args)
	if err != nil {
		return tools.Result{}, err
	}
	// 产品查询的哈希在 worker 内计算。父进程打开后再关闭同一个 inode 的
	// 任意 fd，会释放该进程在该文件上的 POSIX 锁，可能破坏正在写入的工作区连接。
	// 父进程只看 -wal 是否出现，不打开业务库文件。
	before := presenceSnapshot(t.path)
	if t.local || t.after != nil {
		before, err = Snapshot(t.path)
		if err != nil {
			return tools.Result{}, ErrFileChanged
		}
	}
	// Open 之后才出现 -wal。旧连接可能还停在打开时的快照上，这次不读。
	if claimNewWAL(t.db, before) {
		dropIdle(t.db)
		return tools.Result{}, busyErr()
	}
	if err := Check(query); err != nil {
		if aerr := t.writeAudit(ctx, call, err.Error()); aerr != nil {
			return tools.Result{}, aerr
		}
		if herr := t.checkLocalFile(before); herr != nil {
			return tools.Result{}, herr
		}
		return tools.Result{}, rejected(err.Error())
	}
	result, qerr := t.query(ctx, t.db, query, limit)
	if t.after != nil {
		t.after()
	}
	if herr := t.checkLocalFile(before); herr != nil {
		return tools.Result{}, herr
	}
	if qerr != nil {
		var toolErr *tools.Error
		if errors.As(qerr, &toolErr) {
			return tools.Result{}, qerr
		}
		var timeout *codedTimeout
		if errors.As(qerr, &timeout) {
			return tools.Result{}, qerr
		}
		if isBusy(qerr) {
			return tools.Result{}, busyErr()
		}
		if errors.Is(qerr, errRowTooLarge) {
			return tools.Result{}, resultTooLarge()
		}
		if errors.Is(qerr, errTooManyColumns) {
			return tools.Result{}, rejected("结果超过 32 列，请少选几列。")
		}
		msg := scrub(qerr.Error(), t.path, query)
		if writeDenied(qerr) {
			if aerr := t.writeAudit(ctx, call, "只读连接拒绝了写入"); aerr != nil {
				return tools.Result{}, aerr
			}
		}
		return tools.Result{}, rejected("查询无法执行：" + clipRunes(msg, 180))
	}
	body, err := encodeJSON(result)
	if err != nil {
		return tools.Result{}, err
	}
	if len(body) > MaxResultBytes {
		return tools.Result{}, resultTooLarge()
	}
	summary := fmt.Sprintf("返回 %d 行", result.RowCount)
	if result.TruncateReason == TruncateBytes {
		summary += "，结果因字节上限被截断"
	} else if result.Truncated {
		summary += "，结果已截断"
	}
	return tools.Result{Content: string(body), Summary: summary}, nil
}

func (t *Tool) checkLocalFile(before Snap) error {
	if t.local || t.after != nil {
		return t.sameFile(before)
	}
	return nil
}

func presenceSnapshot(path string) Snap {
	_, err := os.Stat(path + "-wal")
	return Snap{Sides: []Side{{Path: path + "-wal", Exists: err == nil}}}
}

func (t *Tool) sameFile(before Snap) error {
	after, err := Snapshot(t.path)
	if err != nil || before.Unchanged(after) != nil {
		if err == nil && !hasWAL(before) && hasWAL(after) {
			adoptGeneration(t.db, after)
		}
		dropIdle(t.db)
		return busyErr()
	}
	return nil
}

func (t *Tool) writeAudit(ctx context.Context, call tools.Call, detail string) error {
	if t.log == nil {
		return nil
	}
	return t.log.Append(ctx, Event{
		Actor:  "run_sql",
		Action: CodeRejected,
		RunID:  call.RunID,
		Detail: detail,
	})
}

// rejected 把语句错误交回模型，让它改写查询。
// Retryable 为 false：同一条 SQL 不会自动重跑。
// 这不是参数失败。循环既不把它计入连续失败，也不因此把连续失败清零。
func rejected(message string) error {
	return &tools.Error{Code: CodeRejected, Retryable: false, Message: message}
}

// resultTooLarge 把单行超限交回模型，让它少选几列或用 substr 截短。
// Retryable 为 false：同一条 SQL 不会自动重跑。结果里没有任何行。
func resultTooLarge() error {
	return &tools.Error{
		Code:      CodeResultTooLarge,
		Retryable: false,
		Message:   "查询或结果超过资源上限，这次没有返回任何行。请减少中间结果、少选几列，或用 substr 截短字段。",
	}
}

// busyErr 是可重试的 data_busy。结果已经丢掉，消息里不带行，也不带路径。
func busyErr() error {
	return &tools.Error{
		Code:      CodeBusy,
		Retryable: true,
		Message:   "分析库正在写入，这次没有返回结果",
	}
}

func parseArgs(raw []byte) (string, int, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", 0, invalidArg("参数不是合法 JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var req struct {
		SQL   string `json:"sql"`
		Limit *int   `json:"limit"`
	}
	if err := dec.Decode(&req); err != nil {
		return "", 0, invalidArg("参数不符合 Schema")
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", 0, invalidArg("参数不是合法 JSON")
	}
	if req.SQL == "" {
		return "", 0, invalidArg("参数不符合 Schema")
	}
	limit := DefaultLimit
	if req.Limit != nil {
		if *req.Limit < 1 || *req.Limit > MaxLimit {
			return "", 0, invalidArg("参数不符合 Schema")
		}
		limit = *req.Limit
	}
	return req.SQL, limit, nil
}

func invalidArg(message string) error {
	return &tools.Error{Code: tools.CodeArgumentsInvalid, Retryable: true, Message: message}
}

func writeDenied(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "readonly") ||
		strings.Contains(msg, "read-only") ||
		strings.Contains(msg, "read only") ||
		strings.Contains(msg, "query_only") ||
		strings.Contains(msg, "not authorized")
}

// 驱动报错会把 SQL 和文件路径带回来。给模型的文本里这两样都不留。
var pathLike = regexp.MustCompile(`(?:[A-Za-z]:[\\/]|/)[^\s'"]+`)

func scrub(msg, path, query string) string {
	if query != "" {
		msg = strings.ReplaceAll(msg, query, "查询")
	}
	abs, absErr := filepath.Abs(path)
	resolved, resErr := Resolve(path)
	var paths []string
	if path != "" {
		paths = append(paths, path, filepath.Dir(path))
	}
	if absErr == nil {
		paths = append(paths, abs, filepath.Dir(abs))
	}
	if resErr == nil {
		paths = append(paths, resolved, filepath.Dir(resolved))
	}
	for _, p := range paths {
		if p == "" || p == "." || p == string(filepath.Separator) {
			continue
		}
		msg = strings.ReplaceAll(msg, p, "分析库")
		slash := filepath.ToSlash(p)
		if slash != p {
			msg = strings.ReplaceAll(msg, slash, "分析库")
		}
	}
	return pathLike.ReplaceAllString(msg, "分析库")
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// 给测试用，确认执行后的哈希核对真的跑了。生产调用保持 nil。
func (t *Tool) setAfterQuery(fn func()) {
	if t != nil {
		t.after = fn
	}
}
