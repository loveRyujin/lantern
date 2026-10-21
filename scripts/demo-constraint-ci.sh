#!/usr/bin/env bash
# 用和 CI 相同的命令，在临时副本里证明这些约束：
#   1. 引入只含 cgo 的依赖后，CGO_ENABLED=0 go build ./... 失败
#   2. internal/ui 之外 import github.com/egoist/mygo 后，golangci-lint (depguard) 失败
#   3. core/internal/ui 这种路径片段同样被拒绝；模块根下的 internal/ui 及其子包仍然通过
# 该失败的命令失败、该通过的命令通过时，本脚本成功退出。
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if ! command -v go >/dev/null 2>&1; then
	echo "需要 go" >&2
	exit 1
fi
if ! command -v golangci-lint >/dev/null 2>&1; then
	echo "需要 golangci-lint v2" >&2
	exit 1
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

copy_tree() {
	local dest="$1"
	mkdir -p "$dest"
	tar -C "$root" --exclude .git --exclude bin -cf - . | tar -C "$dest" -xf -
}

expect_fail() {
	local title="$1"
	local pattern="$2"
	local dir="$3"
	shift 3

	echo
	echo "======== ${title} ========"
	local log
	log="$(mktemp)"
	set +e
	(cd "$dir" && "$@") >"$log" 2>&1
	local status=$?
	set -e
	cat "$log"
	if [[ "$status" -eq 0 ]]; then
		echo "结果不对: 命令成功了，但这里应该失败。" >&2
		exit 1
	fi
	if ! grep -Eq "$pattern" "$log"; then
		echo "结果不对: 命令失败了，但输出没有匹配 /${pattern}/。" >&2
		exit 1
	fi
	echo "符合预期: 退出码 ${status}，输出匹配 /${pattern}/。"
	rm -f "$log"
}

expect_ok() {
	local title="$1"
	local dir="$2"
	shift 2

	echo
	echo "======== ${title} ========"
	local log
	log="$(mktemp)"
	set +e
	(cd "$dir" && "$@") >"$log" 2>&1
	local status=$?
	set -e
	cat "$log"
	if [[ "$status" -ne 0 ]]; then
		echo "结果不对: 命令失败了，但这里应该成功。" >&2
		exit 1
	fi
	echo "符合预期: 退出码 0。"
	rm -f "$log"
}

cgo_dir="${work}/cgo"
copy_tree "$cgo_dir"
mkdir -p "${cgo_dir}/internal/cgodep"
cat >"${cgo_dir}/internal/cgodep/go.mod" <<'EOF'
module github.com/loveRyujin/cgo-dep

go 1.27.1
EOF
cat >"${cgo_dir}/internal/cgodep/probe.go" <<'EOF'
package cgodep

/*
#cgo LDFLAGS: -lm
*/
import "C"
EOF
(
	cd "$cgo_dir"
	go mod edit \
		-require=github.com/loveRyujin/cgo-dep@v0.0.0 \
		-replace=github.com/loveRyujin/cgo-dep=./internal/cgodep
)
cat >"${cgo_dir}/core/agent/cgo_probe.go" <<'EOF'
package agent

import _ "github.com/loveRyujin/cgo-dep"
EOF

expect_fail \
	"引入 cgo 依赖后，CGO_ENABLED=0 go build ./... 应该失败" \
	"build constraints exclude all Go files" \
	"$cgo_dir" \
	env CGO_ENABLED=0 go build ./...

mygo_dir="${work}/mygo"
copy_tree "$mygo_dir"
cat >"${mygo_dir}/core/events/mygo_probe.go" <<'EOF'
package events

import "github.com/egoist/mygo"

func mygoVersion() string { return mygo.Version }
EOF

expect_fail \
	"internal/ui 之外 import mygo 后，golangci-lint 应该失败" \
	"depguard" \
	"$mygo_dir" \
	golangci-lint run ./...

ui_ok="${work}/ui-ok"
copy_tree "$ui_ok"
mkdir -p "${ui_ok}/internal/ui/sub/nested"
cat >"${ui_ok}/internal/ui/sub/sub.go" <<'EOF'
package sub

import "github.com/egoist/mygo"

func mygoVersion() string { return mygo.Version }
EOF
cat >"${ui_ok}/internal/ui/sub/nested/nested.go" <<'EOF'
package nested

import "github.com/egoist/mygo"

func mygoVersion() string { return mygo.Version }
EOF

expect_ok \
	"模块根下 internal/ui 及其子包 import mygo，golangci-lint 应该通过" \
	"$ui_ok" \
	golangci-lint run ./...

ui_bad="${work}/ui-bad"
copy_tree "$ui_bad"
mkdir -p "${ui_bad}/core/internal/ui"
cat >"${ui_bad}/core/internal/ui/probe.go" <<'EOF'
package ui

import "github.com/egoist/mygo"

func mygoVersion() string { return mygo.Version }
EOF

expect_fail \
	"core/internal/ui 下 import mygo 后，golangci-lint 应该失败" \
	"core/internal/ui" \
	"$ui_bad" \
	golangci-lint run ./...

echo
echo "cgo 和 internal/ui 之外的 mygo 引用都会把对应的 CI 命令打失败；core/internal/ui 同样失败，模块根下的 internal/ui 及其子包仍然通过。"
