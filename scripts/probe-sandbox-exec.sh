#!/bin/sh
# 探测当前机器上的 sandbox-exec。给 GitHub Actions 的 macOS job 用。
# 退出码 0：二进制存在，且 (allow default) 能执行 /usr/bin/true。
# 退出码 1：sandbox-exec 不可用。
# 退出码 2：不是 Darwin，这次探测没有意义。
# deny default、命名 profile 和禁网的结果写进报告，不单独决定退出码。

set -eu

report=$(mktemp)
trap 'rm -f "$report"' EXIT

say() {
	printf '%s\n' "$1" | tee -a "$report"
}

finish() {
	if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
		{
			printf '%s\n' "### sandbox-exec 探测"
			printf '\n'
			printf '%s\n' '```'
			cat "$report"
			printf '%s\n' '```'
		} >> "$GITHUB_STEP_SUMMARY"
	fi
}

os=$(uname -s)
say "uname: $(uname -a)"
say "runner_os: ${RUNNER_OS:-}"
say "image_os: ${ImageOS:-}"
say "image_version: ${ImageVersion:-}"

if [ "$os" != "Darwin" ]; then
	say "结论: 当前不是 macOS（${os}），未探测 sandbox-exec"
	finish
	exit 2
fi

say "product_name: $(sw_vers -productName 2>/dev/null || echo unknown)"
say "product_version: $(sw_vers -productVersion 2>/dev/null || echo unknown)"
say "build_version: $(sw_vers -buildVersion 2>/dev/null || echo unknown)"

bin=""
if command -v sandbox-exec >/dev/null 2>&1; then
	bin=$(command -v sandbox-exec)
fi
say "sandbox_exec: ${bin:-missing}"
if [ -n "$bin" ]; then
	ls -ld "$bin" 2>&1 | tee -a "$report" || true
fi

probe() {
	label=$1
	var=$2
	shift 2
	say "---- ${label} ----"
	if [ -z "$bin" ] || [ ! -x "$bin" ]; then
		say "exit: skipped"
		eval "$var=skipped"
		return 0
	fi
	tmp=$(mktemp)
	set +e
	"$@" >"$tmp" 2>&1
	code=$?
	set -e
	say "exit: ${code}"
	awk 'NR<=50 { print "  " $0 }' "$tmp" | tee -a "$report"
	rm -f "$tmp"
	eval "$var=$code"
}

allow_code="skipped"
deny_code="skipped"
named_code="skipped"
curl_code="skipped"
bare_code="skipped"

probe "sandbox-exec 无参数" bare_code "$bin"
probe "allow default /usr/bin/true" allow_code \
	"$bin" -p '(version 1)(allow default)' /usr/bin/true
probe "deny default /usr/bin/true" deny_code \
	"$bin" -p '(version 1)(deny default)(allow process-exec)(allow process-fork)(allow signal (target self))(allow sysctl-read)(allow file-read*)(allow file-read-metadata)(allow file-map-executable)' /usr/bin/true
probe "named profile no-network /usr/bin/true" named_code \
	"$bin" -n no-network /usr/bin/true

if [ -x /usr/bin/curl ]; then
	probe "deny network curl example.com" curl_code \
		"$bin" -p '(version 1)(allow default)(deny network*)' /usr/bin/curl -fsS --max-time 8 https://example.com
else
	say "---- deny network curl example.com ----"
	say "exit: skipped"
	say "  没有 /usr/bin/curl"
fi

result=1
msg="结论: sandbox-exec 不可用（找不到可执行文件）"
if [ -n "$bin" ] && [ -x "$bin" ]; then
	if [ "$allow_code" = "0" ]; then
		result=0
		msg="结论: sandbox-exec 可用"
	else
		msg="结论: sandbox-exec 存在，但 (allow default) 无法执行 /usr/bin/true"
	fi
fi
say "${msg}; allow_default=${allow_code}; deny_default=${deny_code}; named_no_network=${named_code}; network_deny_curl=${curl_code}; bare=${bare_code}"
finish
exit "$result"
