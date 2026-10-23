.PHONY: seed test eval eval-negative eval-sandbox-real build

export CGO_ENABLED := 0

# eval 运行 lantern-eval。
# MODE=mock|live，CATEGORY 可选，PLATFORM=auto|darwin|linux|all。
# EVAL_DIR 是用例目录，REPORT_DIR 是报告目录。
# RULES 是规则清单（默认 EVAL_DIR/rules.yaml），GATES 是门槛文件（默认 EVAL_DIR/gates.yaml）。
# 用例失败、缺观测或硬门槛未达标（含无样本）时，本目标以非零状态退出。
MODE ?= mock
PLATFORM ?= auto
EVAL_DIR ?= eval/v1
REPORT_DIR ?= reports
RULES ?=
GATES ?=
EVAL_RUNTIME_CONFIG ?=
# 故意违规不进正式集。规则和门槛仍用 eval/v1。
# .yaml.disabled 默认不被扫描，本目标会把它复制成 .yaml 再跑。
NEG_FIXTURES ?= eval/v1/negative-fixtures/cases.yaml.disabled
NEG_VIOLATIONS ?= internal/eval/testdata/violations
NEG_RULES ?= eval/v1/rules.yaml
NEG_GATES ?= eval/v1/gates.yaml

seed:
	go run ./cmd/lantern-seed -force

test:
	go test ./...

eval:
	go run ./cmd/lantern-eval run --mode="$(MODE)" --platform="$(PLATFORM)" --root="$(EVAL_DIR)" --out="$(REPORT_DIR)" $(if $(CATEGORY),--category="$(CATEGORY)",) $(if $(RULES),--rules="$(RULES)",) $(if $(GATES),--gates="$(GATES)",)

# Preparation is explicit: lantern-eval prepare-runtime --root <dir> --out <new.json>.
# This target only consumes that prepared configuration and executes real probes.
eval-sandbox-real:
	@test -n "$(EVAL_RUNTIME_CONFIG)" || { echo "需要 EVAL_RUNTIME_CONFIG=<已准备的Runtime.json>" >&2; exit 2; }
	go run ./cmd/lantern-eval run --mode=sandbox-real --platform=linux --root=eval/sandbox-real/v1 --runtime-config="$(EVAL_RUNTIME_CONFIG)" --out="$(REPORT_DIR)"

# eval-negative 同时跑 negative-fixtures/cases.yaml.disabled 和 testdata/violations。
# 三道门槛都应失败，因此本目标非零退出。
eval-negative:
	@set -e; \
	dir=$$(mktemp -d); \
	trap 'rm -rf "$$dir"' EXIT; \
	cp "$(NEG_VIOLATIONS)/exfiltration/case.yaml" "$$dir/violate-exfiltration.yaml"; \
	cp "$(NEG_VIOLATIONS)/side_effect/case.yaml" "$$dir/violate-side-effect.yaml"; \
	cp "$(NEG_VIOLATIONS)/sandbox/case.yaml" "$$dir/violate-sandbox.yaml"; \
	cp "$(NEG_VIOLATIONS)/approval_bypass/case.yaml" "$$dir/violate-approval-bypass.yaml"; \
	cp "$(NEG_VIOLATIONS)/cross_workspace/case.yaml" "$$dir/violate-cross-workspace.yaml"; \
	cp "$(NEG_FIXTURES)" "$$dir/negative-fixtures.yaml"; \
	go run ./cmd/lantern-eval run --mode="$(MODE)" --platform="$(PLATFORM)" --root="$$dir" --out="$(REPORT_DIR)" --rules="$(NEG_RULES)" --gates="$(NEG_GATES)" $(if $(CATEGORY),--category="$(CATEGORY)",)

build:
	go build ./...
