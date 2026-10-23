//go:build linux && amd64

package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// prepareFontCache runs only fixed trusted runtime preparation, never Spec.Code.
// Matplotlib starts a warning Timer when building its font cache. Prebuilding
// that cache preserves the strict no-threads execution profile. Only bundled
// fonts enter the seed; host font paths are discarded before serialization.
func prepareFontCache(ctx context.Context, python string) (map[string][]byte, error) {
	root, err := os.MkdirTemp("", "lantern-font-prep-")
	if err != nil {
		return nil, RuntimeUnavailable(err.Error())
	}
	defer os.RemoveAll(root)
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, python, "-I", "-B", "-c", `import importlib.util,json,sys
if importlib.util.find_spec('matplotlib') is None:
 print('{}');sys.exit(0)
from pathlib import Path
import matplotlib as mpl
import matplotlib.font_manager as fm
bundle=Path(mpl.get_data_path()).resolve()
def bundled(entry):
 return Path(entry.fname).resolve().is_relative_to(bundle)
fm.fontManager.ttflist=[entry for entry in fm.fontManager.ttflist if bundled(entry)]
fm.fontManager.afmlist=[entry for entry in fm.fontManager.afmlist if bundled(entry)]
assert fm.fontManager.ttflist,'no bundled TrueType fonts'
cache=Path(mpl.get_cachedir())/('fontlist-v%s.json'%fm.FontManager.__version__)
fm.json_dump(fm.fontManager,cache)
print(json.dumps({cache.name:cache.read_text()}))`)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "PYTHONDONTWRITEBYTECODE=1", "MPLBACKEND=Agg", "MPLCONFIGDIR=" + root, "HOME=" + root, "OPENBLAS_NUM_THREADS=1", "OMP_NUM_THREADS=1", "MKL_NUM_THREADS=1", "NUMEXPR_NUM_THREADS=1"}
	output := cappedStream{limit: 1 << 20}
	var diagnostic cappedStream
	cmd.Stdout = &output
	cmd.Stderr = &diagnostic
	if err = cmd.Run(); err != nil {
		return nil, RuntimeUnavailable(fmt.Sprintf("trusted font-cache preparation: %v %s", err, diagnostic.String()))
	}
	if output.overflow.Load() {
		return nil, RuntimeUnavailable("font cache exceeds 1 MiB")
	}
	var decoded map[string]string
	if err = json.Unmarshal([]byte(output.String()), &decoded); err != nil {
		return nil, RuntimeUnavailable("invalid font-cache metadata")
	}
	seeds := make(map[string][]byte, len(decoded))
	for name, data := range decoded {
		if !validFontSeed(name, []byte(data)) {
			return nil, RuntimeUnavailable("invalid font-cache seed")
		}
		seeds[name] = []byte(data)
	}
	return seeds, nil
}
func validFontSeed(name string, data []byte) bool {
	return filepath.Base(name) == name && strings.HasPrefix(name, "fontlist-v") && strings.HasSuffix(name, ".json") && len(data) <= 1<<20 && json.Valid(data)
}
