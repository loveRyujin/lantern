package errcode

import "errors"

// Code 是稳定错误码。零值表示「没有错误码」。
type Code string

const (
	// Timeout 表示墙钟超时，或执行上下文被取消后进程组已被终止。
	Timeout Code = "timeout"
	// OOM 表示内存超过上限（含父进程按 RSS 轮询后杀掉的情况）。
	OOM Code = "oom"
	// Killed 表示进程被信号杀死，且不是 timeout / oom 这两类已知原因。
	Killed Code = "killed"
	// Exec 表示沙箱内程序非零退出，或其他未归类的执行失败。码值固定为 "error"。
	Exec Code = "error"
	// RuntimeUnavailable 表示 Python 运行时缺失或版本不符。
	// 调用方不得降级为不经沙箱直接执行。
	RuntimeUnavailable Code = "runtime_unavailable"
	// SandboxUnavailable 表示当前环境提供不了所需隔离（例如内核不支持 Landlock）。
	// 调用方不得静默降级到更弱的隔离。
	SandboxUnavailable Code = "sandbox_unavailable"
	// EmbeddingMismatch 表示索引记录的嵌入模型与当前 Embedder 不一致。
	// 比对同时看模型 ID 和维度。调用方不得混用两套向量，也不得自动回退。
	EmbeddingMismatch Code = "embedding_mismatch"
)

// String 返回码值本身。
func (c Code) String() string { return string(c) }

// Known 报告 c 是不是本包定义的稳定错误码。
func (c Code) Known() bool {
	switch c {
	case Timeout, OOM, Killed, Exec, RuntimeUnavailable, SandboxUnavailable, EmbeddingMismatch:
		return true
	default:
		return false
	}
}

// Coded 是带稳定错误码的错误。
type Coded interface {
	error
	Code() Code
}

// CodeOf 取出 err 链上的稳定错误码。
// 没有码、或码不是已知值时，ok 为 false。
func CodeOf(err error) (Code, bool) {
	if err == nil {
		return "", false
	}
	var coded Coded
	if !errors.As(err, &coded) || coded == nil {
		return "", false
	}
	code := coded.Code()
	if !code.Known() {
		return "", false
	}
	return code, true
}
