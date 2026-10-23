package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/loveRyujin/lantern/core/errcode"
)

// Sandbox 在隔离环境里执行一次代码。
//
// Run 返回的 error 为 nil 时，Result.ErrKind 为空，产物已通过白名单。
// error 非 nil 且属于执行失败时，它实现 errcode.Coded，Result.ErrKind 与 Code() 相同。
//
// 网络不是 Spec 的字段。MVP 不能通过确认把网络放开；实现不得提供这样的开关。
// 超出默认权限的额外只读目录放在 Spec.ExtraReadOnly，必须先经过 Policy 和 Approver。
// Sandbox 本身不弹确认。
type Sandbox interface {
	Run(ctx context.Context, spec Spec) (Result, error)
}

// Spec 是一次隔离执行的输入。
//
// Inputs 的键是沙箱内的相对路径（用 / 分隔），值是文件内容。
// 调用方在 Run 返回之前不得修改 Inputs 里的切片。
type Spec struct {
	// Code 是要执行的源码。空字符串不合法。
	Code string
	// Inputs 是放进只读输入目录的文件。
	Inputs map[string][]byte
	// Limits 是本次资源上限。零值表示交给平台实现的默认值，仍受硬上限约束。
	Limits Limits
	// ExtraReadOnly 是额外挂载的宿主机只读目录，必须是干净的绝对路径。
	ExtraReadOnly []string
	// Runtime 描述 Python 解释器、标准库目录和专用 venv。
	// 路径为空时由平台实现探测；探测失败必须返回 runtime_unavailable。
	Runtime Runtime
}

// Runtime 是沙箱内可见的 Python 运行时。三个路径都只读挂载。
type Runtime struct {
	// Python 是解释器绝对路径。空表示由平台实现探测。
	Python string
	// PyRoot 是标准库所在目录。
	PyRoot string
	// Venv 是专用虚拟环境目录。
	Venv string
}

// Limits 是单次执行的资源上限。
//
// 时间为非负值。Memory 和 NProc 的单位分别是字节和进程数，0 表示用默认值。
// MaxOutput 是单个产物的字节上限，0 表示用 MaxArtifactBytes；超过 MaxArtifactBytes 的请求仍按硬上限截断。
type Limits struct {
	// CPU 是 CPU 时间上限。
	CPU time.Duration
	// Memory 是 RSS 上限，单位字节。
	Memory uint64
	// NProc 是进程数上限。
	NProc uint64
	// Wall 是墙钟超时。
	Wall time.Duration
	// MaxOutput 是单个产物的字节上限。
	MaxOutput int64
}

// Result 是一次执行的产出。
type Result struct {
	// Usage reports actual task observations; sampled limits are not hard RSS quotas.
	Usage Usage
	// RuntimeWarnings lists host optional extensions whose dependency closure is unavailable.
	RuntimeWarnings []string
	// Stdout 是标准输出文本。
	Stdout string
	// Stderr 是标准错误文本。
	Stderr string
	// Artifacts 是通过白名单的产物。
	Artifacts []Artifact
	// ErrKind 在失败时给出稳定错误码，成功时为空。
	ErrKind errcode.Code
}

// Artifact 是从输出目录收集的一个文件。
type Artifact struct {
	// Name 是输出目录内的文件名，不能带路径。
	Name string
	// Kind 是白名单类型。
	Kind ArtifactKind
	// Data 是文件内容。
	Data []byte
}

// ArtifactKind 是允许收集的产物类型。
type ArtifactKind string

const (
	// KindPNG 是完整可解码、尺寸受限的 PNG 图片。
	KindPNG ArtifactKind = "png"
	// KindCSV 是 UTF-8 CSV 文本，要求严格引号与一致的字段数。
	KindCSV ArtifactKind = "csv"
	// KindJSON 是 JSON 文本，内容必须能通过 JSON 语法检查。
	KindJSON ArtifactKind = "json"
)

// MaxArtifactBytes 是单个产物的硬上限：5 MiB。
const MaxArtifactBytes int64 = 5 << 20

// ErrInvalidSpec 表示 Spec 在执行前就不符合契约。它不是执行期错误码。
var ErrInvalidSpec = errors.New("sandbox: spec 不合法")

// ErrArtifact 表示产物被白名单拒绝，调用方不应把它交给 UI。
var ErrArtifact = errors.New("sandbox: 产物不在白名单内")

// ErrUnsupported 表示当前平台没有沙箱实现。
// Windows 在 MVP 返回它。它不是稳定错误码，不能当成 timeout 或其他执行失败。
var ErrUnsupported = errors.New("sandbox: 当前平台没有沙箱实现")

// ValidateSpec 检查 spec 能否交给实现。
// Fake 和以后的平台实现都调用它，避免各写一套路径规则。
func ValidateSpec(spec Spec) error {
	if strings.TrimSpace(spec.Code) == "" {
		return fmt.Errorf("%w: 代码为空", ErrInvalidSpec)
	}
	if spec.Limits.CPU < 0 || spec.Limits.Wall < 0 || spec.Limits.MaxOutput < 0 {
		return fmt.Errorf("%w: 资源上限不能为负", ErrInvalidSpec)
	}
	for key := range spec.Inputs {
		if err := validInputKey(key); err != nil {
			return err
		}
	}
	for _, dir := range spec.ExtraReadOnly {
		if err := validHostDir(dir); err != nil {
			return err
		}
	}
	return nil
}

func validInputKey(key string) error {
	if key == "" || key == "." || key == ".." || path.IsAbs(key) || strings.Contains(key, `\`) {
		return fmt.Errorf("%w: 输入路径 %q 不合法", ErrInvalidSpec, key)
	}
	if path.Clean(key) != key || strings.HasPrefix(key, "../") {
		return fmt.Errorf("%w: 输入路径 %q 包含穿越", ErrInvalidSpec, key)
	}
	return nil
}

func validHostDir(dir string) error {
	if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return fmt.Errorf("%w: 额外只读目录 %q 必须是干净的绝对路径", ErrInvalidSpec, dir)
	}
	return nil
}

// ValidateArtifact 按白名单检查一个产物。
//
// limit <= 0 或 limit 大于 MaxArtifactBytes 时，使用 MaxArtifactBytes。
// 合法时返回 nil。文件名只能是输出目录里的基名，扩展名必须和 Kind 一致。
func ValidateArtifact(a Artifact, limit int64) error {
	if err := validArtifactName(a.Name); err != nil {
		return err
	}
	ext := strings.ToLower(path.Ext(a.Name))
	switch a.Kind {
	case KindPNG:
		if ext != ".png" {
			return fmt.Errorf("%w: %s 的扩展名与 png 不一致", ErrArtifact, a.Name)
		}
	case KindCSV:
		if ext != ".csv" {
			return fmt.Errorf("%w: %s 的扩展名与 csv 不一致", ErrArtifact, a.Name)
		}
	case KindJSON:
		if ext != ".json" {
			return fmt.Errorf("%w: %s 的扩展名与 json 不一致", ErrArtifact, a.Name)
		}
	default:
		return fmt.Errorf("%w: 不接受的类型 %q", ErrArtifact, a.Kind)
	}
	limit = artifactLimit(limit)
	size := int64(len(a.Data))
	if size == 0 || size > limit {
		return fmt.Errorf("%w: %s 的大小 %d 不在 1..%d", ErrArtifact, a.Name, size, limit)
	}
	if err := validateArtifactContent(a); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrArtifact, a.Name, err)
	}
	return nil
}

func artifactLimit(limit int64) int64 {
	if limit <= 0 || limit > MaxArtifactBytes {
		return MaxArtifactBytes
	}
	return limit
}

func validArtifactName(name string) error {
	if name == "" || name == "." || name == ".." || strings.Contains(name, `\`) || path.Base(name) != name {
		return fmt.Errorf("%w: 文件名 %q 不合法", ErrArtifact, name)
	}
	return nil
}

func hasPNGSignature(data []byte) bool {
	sig := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	return bytes.HasPrefix(data, sig)
}

func bytesContainNUL(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}
