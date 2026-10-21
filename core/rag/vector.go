package rag

import (
	"container/heap"
	"fmt"
	"math"
)

// L2Normalize 返回 v 的 L2 归一化副本，不修改 v。
// 空向量和零向量返回 ErrZeroVector。含 NaN 或 Inf 时返回 ErrInvalidArgument。
func L2Normalize(v []float32) ([]float32, error) {
	if len(v) == 0 {
		return nil, ErrZeroVector
	}
	if err := finite(v); err != nil {
		return nil, err
	}
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return nil, ErrZeroVector
	}
	inv := 1 / math.Sqrt(sum)
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) * inv)
	}
	return out, nil
}

// Dot 返回点积。两条向量都已 L2 归一化时，点积就是余弦。
// 长度不同返回 DimensionError。
func Dot(a, b []float32) (float32, error) {
	if len(a) != len(b) {
		return 0, &DimensionError{Got: len(a), Want: len(b)}
	}
	if err := finite(a); err != nil {
		return 0, err
	}
	if err := finite(b); err != nil {
		return 0, err
	}
	var sum float64
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return float32(sum), nil
}

func finite(v []float32) error {
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return fmt.Errorf("%w: 向量含有非有限值", ErrInvalidArgument)
		}
	}
	return nil
}

func prepareVector(v []float32, dim int, normalize bool) ([]float32, error) {
	if len(v) != dim {
		return nil, &DimensionError{Got: len(v), Want: dim}
	}
	if !normalize {
		if err := finite(v); err != nil {
			return nil, err
		}
		return append([]float32(nil), v...), nil
	}
	return L2Normalize(v)
}

type scoredHit struct {
	score float32
	id    string
	hit   Hit
}

func worse(a, b scoredHit) bool {
	if a.score != b.score {
		return a.score < b.score
	}
	return a.id > b.id
}

type hitHeap []scoredHit

func (h hitHeap) Len() int { return len(h) }

func (h hitHeap) Less(i, j int) bool { return worse(h[i], h[j]) }

func (h hitHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *hitHeap) Push(x any) { *h = append(*h, x.(scoredHit)) }

func (h *hitHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

// pickTop 用小顶堆保留分数最高的 k 条。
// 分数高的在前；分数相同则 chunk id 字典序较小的在前。
func pickTop(items []scoredHit, k int) []Hit {
	if k <= 0 || len(items) == 0 {
		return nil
	}
	if k > len(items) {
		k = len(items)
	}
	h := &hitHeap{}
	for _, it := range items {
		if h.Len() < k {
			heap.Push(h, it)
			continue
		}
		if worse((*h)[0], it) {
			heap.Pop(h)
			heap.Push(h, it)
		}
	}
	out := make([]Hit, h.Len())
	for i := len(out) - 1; i >= 0; i-- {
		it := heap.Pop(h).(scoredHit)
		it.hit.Score = it.score
		out[i] = it.hit
	}
	return out
}
