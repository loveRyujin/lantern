// Command lantern-eval 是无界面评测入口。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/loveRyujin/lantern/internal/eval"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "run":
			args = args[1:]
		case "help":
			fmt.Fprintln(stdout, "用法: lantern-eval run [选项]")
			return eval.ExitOK
		default:
			fmt.Fprintf(stderr, "未知子命令 %q\n", args[0])
			return eval.ExitUsage
		}
	}

	fs := flag.NewFlagSet("lantern-eval run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mode := fs.String("mode", eval.ModeMock, "运行模式：mock 或 live")
	category := fs.String("category", "", "只跑一个类别；空表示全部")
	platform := fs.String("platform", "auto", "沙箱用例平台：auto、darwin、linux 或 all")
	root := fs.String("root", "eval/v1", "用例目录，递归读取 YAML 和 JSONL")
	out := fs.String("out", "reports", "报告目录，写出 eval-<时间>.md 和 .json")
	rules := fs.String("rules", "", "规则清单路径，默认是用例目录下的 rules.yaml")
	judgeModel := fs.String("judge-model", eval.DefaultJudgeModel, "LLM 裁判模型")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "用法: lantern-eval run [选项]")
		fmt.Fprintln(stderr, "读取 YAML/JSONL 用例并写出 Markdown 和 JSON 报告。用例失败或硬门槛未达标时退出码为 1。")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return eval.ExitOK
		}
		return eval.ExitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "多余参数: %s\n", strings.Join(fs.Args(), " "))
		return eval.ExitUsage
	}

	res, err := eval.Execute(context.Background(), eval.Options{
		Root:       *root,
		OutDir:     *out,
		RulesPath:  *rules,
		Mode:       *mode,
		Category:   *category,
		Platform:   *platform,
		JudgeModel: *judgeModel,
	})
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return eval.ExitUsage
	}
	fmt.Fprintln(stdout, res.JSONPath)
	fmt.Fprintln(stdout, res.MarkdownPath)
	if res.ExitCode != 0 {
		fmt.Fprintf(stderr, "评测未通过（硬门槛通过=%t，失败用例=%d）\n", res.Report.Summary.HardGatesPassed, res.Report.Summary.Failed)
	}
	return res.ExitCode
}
