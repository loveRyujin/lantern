.PHONY: seed test eval build

export CGO_ENABLED := 0

# eval 运行 lantern-eval。
# MODE=mock|live，CATEGORY 可选，PLATFORM=auto|darwin|linux|all。
# EVAL_DIR 是用例目录，REPORT_DIR 是报告目录，RULES 是规则清单（默认 EVAL_DIR/rules.yaml）。
# 用例失败或硬门槛未达标时，本目标以非零状态退出。
MODE ?= mock
PLATFORM ?= auto
EVAL_DIR ?= eval/v1
REPORT_DIR ?= reports
RULES ?=

seed:
	@echo "TODO: lantern-seed 尚未实现"

test:
	go test ./...

eval:
	go run ./cmd/lantern-eval run --mode="$(MODE)" --platform="$(PLATFORM)" --root="$(EVAL_DIR)" --out="$(REPORT_DIR)" $(if $(CATEGORY),--category="$(CATEGORY)",) $(if $(RULES),--rules="$(RULES)",)

build:
	go build ./...
