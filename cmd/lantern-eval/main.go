// Command lantern-eval 是无界面评测入口。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/loveRyujin/lantern/core/pyrt"
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
			fmt.Fprintln(stdout, "用法: lantern-eval run [选项] | prepare-runtime --root <目录> [--out <配置.json>]")
			return eval.ExitOK
		case "prepare-runtime":
			return prepareRuntime(args[1:], stdout, stderr)
		default:
			fmt.Fprintf(stderr, "未知子命令 %q\n", args[0])
			return eval.ExitUsage
		}
	}

	fs := flag.NewFlagSet("lantern-eval run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mode := fs.String("mode", eval.ModeMock, "运行模式：mock、sandbox-real 或 live（未接入）")
	category := fs.String("category", "", "只跑一个类别；空表示全部")
	platform := fs.String("platform", "auto", "沙箱用例平台：auto、darwin、linux 或 all")
	root := fs.String("root", "eval/v1", "用例目录，递归读取 YAML 和 JSONL")
	out := fs.String("out", "reports", "报告目录，写出 eval-<时间>.md 和 .json")
	rules := fs.String("rules", "", "规则清单路径，默认是用例目录下的 rules.yaml")
	gates := fs.String("gates", "", "门槛文件路径，默认是用例目录下的 gates.yaml")
	judgeModel := fs.String("judge-model", eval.DefaultJudgeModel, "LLM 裁判模型")
	runtimeConfig := fs.String("runtime-config", "", "sandbox-real 的已准备 Python Runtime JSON；不会自动安装")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "用法: lantern-eval run [选项]")
		fmt.Fprintln(stderr, "读取用例并写出 Markdown 和 JSON 报告。失败、缺观测或硬门槛未达标时退出 1。sandbox-real 仅验收真实 Linux 沙箱，默认独立 suite；全项目 live 尚未接入。")
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
	var prepared *pyrt.Runtime
	if *mode == eval.ModeSandboxReal {
		rootExplicit := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "root" {
				rootExplicit = true
			}
		})
		if !rootExplicit {
			*root = "eval/sandbox-real/v1"
		}
		if *runtimeConfig != "" {
			var err error
			prepared, err = readRuntime(*runtimeConfig)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return eval.ExitUsage
			}
		}
	} else if *runtimeConfig != "" {
		fmt.Fprintln(stderr, "runtime-config 仅用于 sandbox-real")
		return eval.ExitUsage
	}

	res, err := eval.Execute(context.Background(), eval.Options{
		Root:           *root,
		OutDir:         *out,
		RulesPath:      *rules,
		GatesPath:      *gates,
		Mode:           *mode,
		Category:       *category,
		Platform:       *platform,
		JudgeModel:     *judgeModel,
		SandboxRuntime: prepared,
	})
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return eval.ExitUsage
	}
	fmt.Fprintln(stdout, res.JSONPath)
	fmt.Fprintln(stdout, res.MarkdownPath)
	if res.ExitCode != 0 {
		fmt.Fprintf(stderr, "评测未通过（硬门槛通过=%t，失败用例=%d，缺观测=%d）\n", res.Report.Summary.HardGatesPassed, res.Report.Summary.Failed, res.Report.Summary.MissingObservation)
	}
	return res.ExitCode
}

func readRuntime(path string) (*pyrt.Runtime, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("Runtime 配置必须是普通文件")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("无法读取 Runtime 配置: %w", err)
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil || len(body) > 64<<10 {
		return nil, errors.New("Runtime 配置读取失败或超过 64 KiB")
	}
	d := json.NewDecoder(strings.NewReader(string(body)))
	d.DisallowUnknownFields()
	var rt *pyrt.Runtime
	if err := d.Decode(&rt); err != nil {
		return nil, fmt.Errorf("Runtime 配置不合法: %w", err)
	}
	if rt == nil {
		return nil, errors.New("Runtime 配置必须是对象")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, errors.New("Runtime 配置有多余内容")
	}
	return rt, nil
}

// Preparation is an explicit operator action, separate from evaluation/model
// execution. Refuse to overwrite an existing config; publish complete JSON only.
func prepareRuntime(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lantern-eval prepare-runtime", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "显式准备运行时的目录（可能下载哈希锁定依赖）")
	out := fs.String("out", "", "新建 Runtime JSON 路径，默认 <root>/runtime.json，不覆盖")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return eval.ExitOK
		}
		return eval.ExitUsage
	}
	if fs.NArg() != 0 || strings.TrimSpace(*root) == "" {
		fmt.Fprintln(stderr, "prepare-runtime 需要 --root，且不接受位置参数")
		return eval.ExitUsage
	}
	if *out == "" {
		*out = filepath.Join(*root, "runtime.json")
	}
	if _, err := os.Lstat(*out); err == nil || !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(stderr, "Runtime 输出已存在或不可用，不会覆盖")
		return eval.ExitUsage
	}
	rt, err := (pyrt.Config{Root: *root}).Ensure(context.Background())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return eval.ExitFailed
	}
	if err = writeNewRuntime(*out, rt); err != nil {
		fmt.Fprintln(stderr, err)
		return eval.ExitFailed
	}
	fmt.Fprintln(stdout, *out)
	return eval.ExitOK
}

func writeNewRuntime(path string, rt pyrt.Runtime) error {
	body, err := json.MarshalIndent(rt, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".runtime-json-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(append(body, '\n')); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Link(file.Name(), path)
}
