package eval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// ExitOK 表示报告已写出，硬门槛通过，且没有失败用例。
	ExitOK = 0
	// ExitFailed 表示报告已写出，但有用例失败或硬门槛未达标。
	ExitFailed = 1
	// ExitUsage 表示参数、用例或规则清单无法使用，没有可用的计分报告。
	ExitUsage = 2

	ModeMock = "mock"
	ModeLive = "live"
)

// Options 控制一次评测。RulesPath 为空时读取用例目录下的 rules.yaml。
type Options struct {
	Root       string
	OutDir     string
	RulesPath  string
	Mode       string
	Category   string
	Platform   string
	JudgeModel string
	ModuleRoot string
	Now        func() time.Time
	Judge      Judge
}

// Result 是写出的报告和进程退出码。
type Result struct {
	Report       Report
	JSONPath     string
	MarkdownPath string
	ExitCode     int
}

// Execute 加载用例、对照规则清单计分，并写出 Markdown 与 JSON 报告。
// 清单外的确定性规则（含 _not 形式）会使对应用例失败，退出码为 ExitFailed。
func Execute(ctx context.Context, opt Options) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if opt.Root == "" {
		return Result{}, fmt.Errorf("eval: 用例目录不能为空")
	}
	mode := opt.Mode
	if mode == "" {
		mode = ModeMock
	}
	if mode != ModeMock && mode != ModeLive {
		return Result{}, fmt.Errorf("eval: mode 必须是 mock 或 live")
	}
	opt.Mode = mode
	switch opt.Platform {
	case "", "auto", "all", "darwin", "linux":
	default:
		return Result{}, fmt.Errorf("eval: platform 必须是 auto、darwin、linux 或 all")
	}
	model := opt.JudgeModel
	if model == "" {
		model = DefaultJudgeModel
	}
	opt.JudgeModel = model

	root, err := filepath.Abs(opt.Root)
	if err != nil {
		return Result{}, fmt.Errorf("eval: 用例目录: %w", err)
	}
	rulesPath := opt.RulesPath
	if rulesPath == "" {
		rulesPath = filepath.Join(root, "rules.yaml")
	}
	rulesPath, err = filepath.Abs(rulesPath)
	if err != nil {
		return Result{}, fmt.Errorf("eval: 规则清单路径: %w", err)
	}
	outDir := opt.OutDir
	if outDir == "" {
		outDir = "reports"
	}
	outDir, err = filepath.Abs(outDir)
	if err != nil {
		return Result{}, fmt.Errorf("eval: 报告目录: %w", err)
	}

	suite, err := LoadSuite(root, rulesPath)
	if err != nil {
		return Result{}, err
	}
	catalog, err := loadCatalog(rulesPath, len(suite.Cases) > 0)
	if err != nil {
		return Result{}, err
	}
	if catalog.Keys == nil {
		catalog.Keys = []string{}
	}

	judge := opt.Judge
	if judge == nil {
		judge, err = NewJudge(mode, model)
		if err != nil {
			return Result{}, err
		}
	}

	version := suite.Version
	if catalog.Version != "" {
		version = catalog.Version
	}
	moduleRoot := opt.ModuleRoot
	if moduleRoot == "" {
		moduleRoot = detectModuleRoot()
	}

	now := time.Now
	if opt.Now != nil {
		now = opt.Now
	}
	generated := now().UTC()

	report := scoreSuite(ctx, suite, catalog, opt, judge, model)
	report.SuiteVersion = version
	report.GeneratedAt = generated.Format(time.RFC3339Nano)
	report.Environment = collectEnvironment(mode, model, version, suite.Root, moduleRoot)
	report.RuleCatalog = CatalogInfo{
		Path:    rulesPath,
		Loaded:  catalog.Loaded,
		Version: catalog.Version,
		Keys:    catalog.Keys,
	}
	report, err = report.withDigest()
	if err != nil {
		return Result{}, err
	}
	jsonPath, mdPath, err := writeReportFiles(outDir, generated, report)
	if err != nil {
		return Result{}, err
	}
	code := ExitOK
	if !report.Summary.HardGatesPassed || report.Summary.Failed > 0 {
		code = ExitFailed
	}
	return Result{
		Report:       report,
		JSONPath:     jsonPath,
		MarkdownPath: mdPath,
		ExitCode:     code,
	}, nil
}

func loadCatalog(path string, required bool) (RuleCatalog, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if required {
				return RuleCatalog{}, fmt.Errorf("eval: 用例目录有用例，但规则清单不存在: %s", path)
			}
			return RuleCatalog{Path: path, Keys: []string{}}, nil
		}
		return RuleCatalog{}, fmt.Errorf("eval: 规则清单 %s: %w", path, err)
	}
	if info.IsDir() {
		return RuleCatalog{}, fmt.Errorf("eval: 规则清单不是文件: %s", path)
	}
	return LoadRuleCatalog(path)
}

func detectModuleRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	root, err := findModuleRoot(cwd)
	if err != nil {
		return ""
	}
	return root
}
