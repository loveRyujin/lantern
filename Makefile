.PHONY: seed test eval eval-negative build

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
# 故意违规夹具不在正式集里。规则和门槛仍用 eval/v1，三道门槛都应失败。
NEG_EVAL_DIR ?= internal/eval/testdata/violations
NEG_RULES ?= eval/v1/rules.yaml
NEG_GATES ?= eval/v1/gates.yaml

seed:
	go run ./cmd/lantern-seed -force

test:
	go test ./...

eval:
	go run ./cmd/lantern-eval run --mode="$(MODE)" --platform="$(PLATFORM)" --root="$(EVAL_DIR)" --out="$(REPORT_DIR)" $(if $(CATEGORY),--category="$(CATEGORY)",) $(if $(RULES),--rules="$(RULES)",) $(if $(GATES),--gates="$(GATES)",)

# eval-negative 只跑 testdata/violations。这些用例期望 0、观测为违规值。
# .yaml.disabled 不会被加载。门槛失败时本目标非零退出。
eval-negative:
	go run ./cmd/lantern-eval run --mode="$(MODE)" --platform="$(PLATFORM)" --root="$(NEG_EVAL_DIR)" --out="$(REPORT_DIR)" --rules="$(NEG_RULES)" --gates="$(NEG_GATES)" $(if $(CATEGORY),--category="$(CATEGORY)",)

build:
	go build ./...
