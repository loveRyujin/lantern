package rag

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/loveRyujin/lantern/core/errcode"
)

// Embedder 把文本变成向量。
//
// ID 是稳定标识，例如 "ollama:bge-m3"、"openai:text-embedding-3-small"。
// Dim 是向量维度。打开工作区时必须同时比对 ID 和 Dim，不能只比维度。
// Embed 按 texts 的顺序返回向量，每条向量的长度都是 Dim。
// 返回的向量应当已经 L2 归一化；FakeEmbedder 会归一化。
type Embedder interface {
	ID() string
	Dim() int
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// EmbedderIdentity 是参与索引比对的模型身份。
type EmbedderIdentity struct {
	// ID 是模型标识，例如 ollama:bge-m3。
	ID string
	// Dim 是向量维度。比对时不能忽略。
	Dim int
}

// Validate 要求 ID 非空且维度为正。
func (id EmbedderIdentity) Validate() error {
	if id.ID == "" || id.Dim <= 0 {
		return fmt.Errorf("%w: embedder 身份不完整", ErrInvalidArgument)
	}
	return nil
}

// IdentityOf 读取 Embedder 的可比对身份。e 为 nil 时返回零值。
func IdentityOf(e Embedder) EmbedderIdentity {
	if e == nil {
		return EmbedderIdentity{}
	}
	return EmbedderIdentity{ID: e.ID(), Dim: e.Dim()}
}

// ErrEmbedderMismatch 是嵌入模型不一致的哨兵。
//
// errors.Is(err, ErrEmbedderMismatch) 对任何 *EmbedderMismatchError 都为真。
// errors.As 取出 Have 和 Want。Have 是索引里的身份，Want 是当前要使用的身份。
// Code 返回 embedding_mismatch。调用方应提示重建索引，不得返回检索结果，也不得回退到另一套向量。
var ErrEmbedderMismatch = &EmbedderMismatchError{}

// EmbedderMismatchError 是一次具体的模型不一致。
type EmbedderMismatchError struct {
	// Have 是索引里记下的身份。
	Have EmbedderIdentity
	// Want 是当前要使用的身份。
	Want EmbedderIdentity
}

// Error 实现 error。零值只返回码值，便于日志识别哨兵。
func (e *EmbedderMismatchError) Error() string {
	if e == nil || (e.Have == (EmbedderIdentity{}) && e.Want == (EmbedderIdentity{})) {
		return string(errcode.EmbeddingMismatch)
	}
	return fmt.Sprintf("%s: 索引是 %s（%d 维），当前是 %s（%d 维）",
		errcode.EmbeddingMismatch, e.Have.ID, e.Have.Dim, e.Want.ID, e.Want.Dim)
}

// Code 返回 embedding_mismatch。
func (e *EmbedderMismatchError) Code() errcode.Code {
	return errcode.EmbeddingMismatch
}

// Is 让所有模型不一致错误都匹配哨兵 ErrEmbedderMismatch。
func (e *EmbedderMismatchError) Is(target error) bool {
	return e != nil && target == ErrEmbedderMismatch
}

// ErrInvalidArgument 表示调用参数不满足接口契约。它不是稳定错误码。
var ErrInvalidArgument = errors.New("rag: 参数不合法")

// ErrDimension 表示向量长度和索引维度不一致。
// 这和 embedding_mismatch 不同：模型身份可以一致，但这条向量本身坏了。
var ErrDimension = errors.New("rag: 向量维度与索引不一致")

// DimensionError 带有实际长度和索引维度。
type DimensionError struct {
	// Got 是这条向量的长度。
	Got int
	// Want 是索引要求的长度。
	Want int
}

// Error 实现 error。
func (e *DimensionError) Error() string {
	if e == nil {
		return ErrDimension.Error()
	}
	return fmt.Sprintf("rag: 向量维度是 %d，索引维度是 %d", e.Got, e.Want)
}

// Is 匹配哨兵 ErrDimension。
func (e *DimensionError) Is(target error) bool {
	return e != nil && target == ErrDimension
}

// ErrZeroVector 表示向量长度为 0 或范数为 0，无法做 L2 归一化。
var ErrZeroVector = errors.New("rag: 零向量不能归一化")

// FakeEmbedder 是确定性 Embedder。相同文本、相同维度得到相同向量，不访问网络。
type FakeEmbedder struct {
	id  string
	dim int
}

var _ Embedder = (*FakeEmbedder)(nil)

// NewFakeEmbedder 构造 fake。id 为空时使用 "fake:deterministic"。dim 必须为正。
func NewFakeEmbedder(id string, dim int) (*FakeEmbedder, error) {
	if id == "" {
		id = "fake:deterministic"
	}
	ident := EmbedderIdentity{ID: id, Dim: dim}
	if err := ident.Validate(); err != nil {
		return nil, err
	}
	return &FakeEmbedder{id: id, dim: dim}, nil
}

// ID 返回构造时的标识。
func (f *FakeEmbedder) ID() string { return f.id }

// Dim 返回构造时的维度。
func (f *FakeEmbedder) Dim() int { return f.dim }

// Embed 按输入顺序返回 L2 归一化向量。texts 为空时返回 nil, nil。
// ctx 已取消时不计算向量。
func (f *FakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if ctx == nil {
		return nil, errors.New("rag: ctx 不能为 nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out[i] = hashVector(f.dim, text)
	}
	return out, nil
}

func hashVector(dim int, text string) []float32 {
	raw := make([]float32, dim)
	filled := 0
	var counter uint32
	for filled < dim {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", counter, text)))
		counter++
		for i := 0; i+4 <= len(sum) && filled < dim; i += 4 {
			u := binary.LittleEndian.Uint32(sum[i : i+4])
			raw[filled] = float32(int32(u)) / float32(math.MaxInt32)
			filled++
		}
	}
	out, err := L2Normalize(raw)
	if err != nil {
		out = make([]float32, dim)
		out[0] = 1
		return out
	}
	return out
}
