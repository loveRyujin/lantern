package agent

import (
	"fmt"
	"strings"

	"github.com/loveRyujin/lantern/core/rag"
)

const (
	// DefaultCitationRewrites 是最终回答没通过校验时，额外给模型的重写次数。
	//
	// 1 表示第一次回答不合格后还能再交一次最终回答。第二次仍不合格就以
	// citation_invalid 结束，不把带伪造引用的正文当成完成。
	// 中间如果模型又去调用工具，那些调用不算把重写次数还回去。
	// 重写本身占用一次模型步数，步数用尽时运行以 max_steps 结束，同样不会放行不合格回答。
	DefaultCitationRewrites = 1

	citeOpen  = "[[cite:"
	citeClose = "]]"
)

// runRetrieval 记下本次运行里 search_docs 实际返回、因而可以引用的 chunk_id。
// 嵌入失败或工具报错不记入。低于阈值的片段也不记入。
type runRetrieval struct {
	ids        map[string]struct{}
	searches   int
	noEvidence int
}

func (r *runRetrieval) observe(content string) {
	if r == nil {
		return
	}
	ids, noEvidence, ok := rag.EvidenceFromSearch(content)
	if !ok {
		return
	}
	r.searches++
	if noEvidence {
		r.noEvidence++
		return
	}
	if r.ids == nil {
		r.ids = map[string]struct{}{}
	}
	for _, id := range ids {
		r.ids[id] = struct{}{}
	}
}

// check 检查最终回答。ok 为 false 时 feedback 是打回给模型的说明。
func (r *runRetrieval) check(answer string) (feedback string, ok bool) {
	var allowed map[string]struct{}
	if r != nil {
		allowed = r.ids
	}
	missing := missingCitations(answer, allowed)
	refuse := r != nil && r.searches > 0 && r.noEvidence == r.searches && len(r.ids) == 0 &&
		!strings.Contains(answer, rag.NoEvidenceMessage)
	if len(missing) == 0 && !refuse {
		return "", true
	}
	var b strings.Builder
	switch {
	case len(missing) > 0 && refuse:
		b.WriteString("引用校验未通过，且本次检索没有可引用的依据。")
	case len(missing) > 0:
		b.WriteString("引用校验未通过。")
	default:
		b.WriteString("无依据拒答未通过。")
	}
	if len(missing) > 0 {
		b.WriteString("这些 chunk_id 不在本次检索结果中：")
		b.WriteString(formatMissing(missing))
		b.WriteString("。")
	}
	if refuse {
		b.WriteString("本次检索未找到依据，最终回答必须写明「未找到依据」，不要编造口径或数值。")
	}
	return b.String(), false
}

const citationRewriteAsk = "请重写最终回答。引用只能使用 [[cite:chunk_id]]，且 chunk_id 必须来自本次 search_docs 已返回的片段。这是仅有的一次重写机会。"

func formatMissing(ids []string) string {
	const maxIDs = 8
	if len(ids) <= maxIDs {
		return strings.Join(ids, "、")
	}
	return strings.Join(ids[:maxIDs], "、") + fmt.Sprintf(" 等 %d 个", len(ids))
}

func missingCitations(answer string, allowed map[string]struct{}) []string {
	var missing []string
	seen := map[string]struct{}{}
	rest := answer
	for {
		i := strings.Index(rest, citeOpen)
		if i < 0 {
			return missing
		}
		rest = rest[i+len(citeOpen):]
		j := strings.Index(rest, citeClose)
		if j < 0 {
			return missing
		}
		id := rest[:j]
		rest = rest[j+len(citeClose):]
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if _, ok := allowed[id]; ok {
			continue
		}
		missing = append(missing, id)
	}
}
