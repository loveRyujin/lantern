package rag_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/loveRyujin/lantern/core/rag"
)

func TestSplitFixedWindowAndOverlap(t *testing.T) {
	parts, err := rag.SplitFixed("abcdef", 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[0].Text != "abcd" || parts[1].Text != "cdef" {
		t.Fatalf("窗口 = %+v", texts(parts))
	}
	if parts[0].Strategy != rag.StrategyFixed || parts[0].TitlePath != "" || parts[1].TitlePath != "" {
		t.Fatalf("元数据 = %+v", parts)
	}
	assertOverlap(t, parts[0].Text, parts[1].Text, 2)

	short, err := rag.SplitFixed("  abc  ", 8, 2)
	if err != nil || len(short) != 1 || short[0].Text != "abc" {
		t.Fatalf("短文本 = %+v err=%v", texts(short), err)
	}
	chars, err := rag.SplitFixed("你好", 1, 0)
	if err != nil || len(chars) != 2 || chars[0].Text != "你" || chars[1].Text != "好" {
		t.Fatalf("应按码点切开: %+v err=%v", texts(chars), err)
	}
	if utf8.RuneCountInString("你") != 1 {
		t.Fatal("测试前提变了")
	}
	folded, err := rag.SplitFixed("a\r\nb", 8, 1)
	if err != nil || len(folded) != 1 || folded[0].Text != "a\nb" {
		t.Fatalf("换行 = %q err=%v", texts(folded), err)
	}
	for _, body := range []string{"", " \n\t", "\uFEFF  \r\n"} {
		got, err := rag.SplitFixed(body, 8, 1)
		if err != nil || got != nil {
			t.Fatalf("空白 %q → %+v err=%v", body, got, err)
		}
	}
	if _, err := rag.SplitFixed("abc", 4, 4); err == nil {
		t.Fatal("重叠等于窗口应该拒绝")
	}
	if _, err := rag.SplitFixed("abc", 0, 0); err == nil {
		t.Fatal("窗口 0 应该拒绝")
	}
}

func TestSplitFixedDefaultRunes(t *testing.T) {
	if rag.FixedWindowRunes != 512 || rag.FixedOverlapRunes != 64 {
		t.Fatalf("默认窗口 = %d/%d", rag.FixedWindowRunes, rag.FixedOverlapRunes)
	}
	runes := make([]rune, 600)
	for i := range runes {
		runes[i] = rune('a' + i%26)
	}
	parts, err := rag.SplitFixed(string(runes), rag.FixedWindowRunes, rag.FixedOverlapRunes)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Fatalf("600 个码点应是 2 段，得到 %d", len(parts))
	}
	if utf8.RuneCountInString(parts[0].Text) != 512 || utf8.RuneCountInString(parts[1].Text) != 152 {
		t.Fatalf("长度 = %d %d", utf8.RuneCountInString(parts[0].Text), utf8.RuneCountInString(parts[1].Text))
	}
	assertOverlap(t, parts[0].Text, parts[1].Text, rag.FixedOverlapRunes)
	for _, part := range parts {
		if part.Strategy != rag.StrategyFixed || part.TitlePath != "" {
			t.Fatalf("fixed 不应带标题路径: %+v", part)
		}
	}
}

func TestSplitStructuralHeadingsAndEntries(t *testing.T) {
	body := "" +
		"# 手册\n" +
		"\n" +
		"引言\n" +
		"\n" +
		"## GMV\n" +
		"\n" +
		"定义\n" +
		"\n" +
		"### 例外\n" +
		"\n" +
		"不算取消\n" +
		"\n" +
		"## 净销售额\n" +
		"\n" +
		"公式\n"
	parts := rag.SplitStructural(body)
	want := []struct{ path, text string }{
		{"手册", "引言"},
		{"手册 / GMV", "定义"},
		{"手册 / GMV / 例外", "不算取消"},
		{"手册 / 净销售额", "公式"},
	}
	if len(parts) != len(want) {
		t.Fatalf("段数 = %d %+v", len(parts), texts(parts))
	}
	for i, part := range parts {
		if part.TitlePath != want[i].path || part.Text != want[i].text || part.Strategy != rag.StrategyStructural {
			t.Fatalf("第 %d 段 = %+v，想要 %+v", i, part, want[i])
		}
	}

	entries := "# 口径\n\n总述\n\n- GMV：已支付订单之和\n- 净销售额: 减去退款\n\n补充\n"
	parts = rag.SplitStructural(entries)
	want = []struct{ path, text string }{
		{"口径", "总述"},
		{"口径 / GMV", "已支付订单之和"},
		{"口径 / 净销售额", "减去退款"},
		{"口径", "补充"},
	}
	if len(parts) != len(want) {
		t.Fatalf("条目 = %+v", parts)
	}
	for i, part := range parts {
		if part.TitlePath != want[i].path || part.Text != want[i].text {
			t.Fatalf("条目 %d = %+v", i, part)
		}
	}

	fenced := "# 文档\n\n可见\n\n```\n# 假标题\n- 假条目：不要切开\n```\n\n## 真标题\n\n内容\n"
	parts = rag.SplitStructural(fenced)
	if len(parts) != 2 {
		t.Fatalf("围栏 = %+v", parts)
	}
	if parts[0].TitlePath != "文档" || !strings.Contains(parts[0].Text, "# 假标题") || !strings.Contains(parts[0].Text, "假条目") {
		t.Fatalf("围栏内不应切分: %+v", parts[0])
	}
	if parts[1].TitlePath != "文档 / 真标题" || parts[1].Text != "内容" {
		t.Fatalf("真标题 = %+v", parts[1])
	}

	closed, errBody := rag.SplitStructural("## 标题 ##\n\n正文\n"), rag.SplitStructural("# C# 语言\n\n保留井号\n")
	if len(closed) != 1 || closed[0].TitlePath != "标题" || closed[0].Text != "正文" {
		t.Fatalf("闭合井号 = %+v", closed)
	}
	if len(errBody) != 1 || errBody[0].TitlePath != "C# 语言" {
		t.Fatalf("标题里的井号 = %+v", errBody)
	}

	plain := rag.SplitStructural("没有标题的一段\n还有第二行\n")
	if len(plain) != 1 || plain[0].TitlePath != "" || !strings.Contains(plain[0].Text, "没有标题") {
		t.Fatalf("无结构 = %+v", plain)
	}
	if rag.SplitStructural("# 只有标题\n") != nil {
		t.Fatal("只有标题不应产出片段")
	}
	if rag.SplitStructural(" \r\n\t") != nil || rag.SplitStructural("") != nil {
		t.Fatal("空白应返回 nil")
	}
	list := rag.SplitStructural("# 手册\n\n- 只是列表\n")
	if len(list) != 1 || list[0].TitlePath != "手册" || !strings.Contains(list[0].Text, "只是列表") {
		t.Fatalf("普通列表 = %+v", list)
	}
}

func TestSplitStructuralHandbook(t *testing.T) {
	body, err := os.ReadFile(repoPath(t, filepath.Join("data", "星禾", "指标口径手册.md")))
	if err != nil {
		t.Fatal(err)
	}
	parts := rag.SplitStructural(string(body))
	anchor := "已支付订单后来发生退款时，GMV 仍按支付金额全额计入"
	var gmv *rag.Chunk
	for i := range parts {
		if parts[i].Strategy != rag.StrategyStructural {
			t.Fatalf("策略 = %s", parts[i].Strategy)
		}
		if parts[i].TitlePath == "星禾电商指标口径手册 / GMV" {
			gmv = &parts[i]
		}
		if strings.TrimSpace(parts[i].Text) == "" {
			t.Fatal("出现空片段")
		}
	}
	if gmv == nil || !strings.Contains(gmv.Text, anchor) {
		t.Fatalf("GMV 片段 = %+v", gmv)
	}
	fixed, err := rag.SplitFixed(string(body), rag.FixedWindowRunes, rag.FixedOverlapRunes)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, part := range fixed {
		if part.TitlePath != "" || part.Strategy != rag.StrategyFixed {
			t.Fatalf("fixed 元数据 = %+v", part)
		}
		if strings.Contains(part.Text, anchor) {
			found = true
		}
	}
	if !found {
		t.Fatal("fixed 窗口丢掉了 GMV 锚点")
	}
}

func assertOverlap(t *testing.T, left, right string, n int) {
	t.Helper()
	lr := []rune(left)
	rr := []rune(right)
	if len(lr) < n || len(rr) < n || string(lr[len(lr)-n:]) != string(rr[:n]) {
		t.Fatalf("重叠 %d：%q 与 %q", n, left, right)
	}
}

func texts(parts []rag.Chunk) []string {
	out := make([]string, len(parts))
	for i, part := range parts {
		out[i] = part.TitlePath + "=" + part.Text
	}
	return out
}

func repoPath(t *testing.T, rel string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("找不到测试文件")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", rel)
}
