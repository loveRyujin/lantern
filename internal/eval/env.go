package eval

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func currentPlatform() string {
	return runtime.GOOS
}

// CheckReportEnvironment 拒绝空的模型、embedder 或沙箱实现。
// embedder_id 和 embedding_model 有一个非空白即可。
func CheckReportEnvironment(env Environment) error {
	if strings.TrimSpace(env.Model) == "" || strings.TrimSpace(env.SandboxImpl) == "" ||
		(strings.TrimSpace(env.EmbedderID) == "" && strings.TrimSpace(env.EmbeddingModel) == "") {
		return fmt.Errorf("eval: 报告环境缺少 model、embedder 或 sandbox_impl")
	}
	return nil
}

func collectEnvironment(judgeModel, suiteVersion, suiteRoot, moduleRoot string) Environment {
	env := Environment{
		OS:             runtime.GOOS,
		Arch:           runtime.GOARCH,
		Platform:       runtime.GOOS,
		JudgeModel:     judgeModel,
		Model:          "mock",
		EmbeddingModel: "mock",
		EmbedderID:     "mock",
		SandboxImpl:    "mock",
		EmbedderDim:    1024,
		VectorStore:    "sqlite-blob",
		ChunkStrategy:  "fixed",
		Commit:         "unknown",
		SuiteVersion:   suiteVersion,
		MygoVersion:    "unknown",
		GoVersion:      runtime.Version(),
		SuiteRoot:      suiteRoot,
	}
	if moduleRoot != "" {
		env.MygoVersion = readMygoVersion(moduleRoot)
		env.Commit = readCommit(moduleRoot)
	}
	if env.JudgeModel == "" {
		env.JudgeModel = DefaultJudgeModel
	}
	return env
}

func findModuleRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

func readMygoVersion(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "unknown"
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "github.com/egoist/mygo" {
			return fields[1]
		}
	}
	return "unknown"
}

func readCommit(root string) string {
	cmd := exec.Command("git", "-C", root, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return "unknown"
	}
	return text
}
