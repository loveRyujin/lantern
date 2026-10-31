package sqlx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// ProcessMemoryBytes 是 SQL worker 的 1 GiB（1,073,741,824 字节）上限数值。
// Linux 把它用作 RLIMIT_AS，限制整个虚拟地址空间，包括 Go 运行时预留，不是 RSS 或 SQLite 堆。
// Darwin 不用 RLIMIT_AS：该常量和 RLIMIT_RSS 是同一个值，而且内核不强制。同一字节数是
// proc_pid_rusage 物理足迹的采样停止线，不是 jetsam 配额，也不是把 1 GiB 放大。
// 两条路径都在读不到停止条件时失败关闭，不退回父进程。
const ProcessMemoryBytes uint64 = 1 << 30

const (
	CodeWorkerFailed      = "sql_worker_failed"
	CodeWorkerUnavailable = "sql_worker_unavailable"
	maxProcessOutput      = MaxResultBytes + 64<<10
	maxProcessInput       = 1 << 20
)

type processRequest struct {
	Path    string
	Allowed string
	SQL     string
	Limit   int
	Device  uint64
	Inode   uint64
}

type processStats struct {
	PID         int    `json:"pid"`
	MemoryLimit uint64 `json:"memory_limit_bytes"`
	PeakRSS     uint64 `json:"peak_rss_bytes"`
	PeakVirtual uint64 `json:"peak_virtual_bytes"`
}

type processMessage struct {
	Version int          `json:"version"`
	Phase   string       `json:"phase"`
	Stats   processStats `json:"stats"`
	Result  *Result      `json:"result,omitempty"`
	Code    string       `json:"code,omitempty"`
}

func encodeJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'}), nil
}

// cappedBuffer 丢弃超限输出，继续排空管道；不能因输出过大把 Wait 卡住。
type cappedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	left := b.limit - b.Len()
	if len(p) > left {
		p = p[:left]
		b.overflow = true
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func parseProcessMessages(body []byte) (processMessage, processMessage, error) {
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	d.DisallowUnknownFields()
	var ready, done processMessage
	if err := d.Decode(&ready); err != nil {
		return ready, done, err
	}
	if ready.Version != 1 || ready.Phase != "ready" || ready.Stats.PID <= 0 || ready.Stats.MemoryLimit != ProcessMemoryBytes {
		return ready, done, errors.New("sqlx: worker 启动信息无效")
	}
	if err := d.Decode(&done); err != nil {
		return ready, done, err
	}
	if done.Version != 1 || done.Phase != "done" || done.Stats.PID != ready.Stats.PID || done.Stats.MemoryLimit != ProcessMemoryBytes {
		return ready, done, errors.New("sqlx: worker 结果信息无效")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return ready, done, errors.New("sqlx: worker 有多余输出")
	}
	return ready, done, nil
}
