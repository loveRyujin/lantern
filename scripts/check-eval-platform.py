#!/usr/bin/env python3
"""Check a lantern-eval report against one platform baseline.

linux: default make eval on Linux (PLATFORM=auto). Scores 213, filters the 20
darwin sandbox cases, skipped is 0. It must not treat all 233 as scored.
darwin: PLATFORM=darwin, mock mode. Loads 233, scores 213, the 20 darwin
sandbox cases all pass.
all: PLATFORM=all. Loads and scores all 233.
"""

import glob
import json
import os
import sys


def main() -> None:
    if len(sys.argv) != 3 or sys.argv[1] not in {"linux", "darwin", "all"}:
        sys.exit("用法: check-eval-platform.py <linux|darwin|all> <报告目录>")
    profile, report_dir = sys.argv[1], sys.argv[2]
    paths = glob.glob(os.path.join(report_dir, "eval-*.json"))
    if len(paths) != 1:
        sys.exit("没有唯一的 JSON 报告: %s" % paths)
    with open(paths[0], encoding="utf-8") as handle:
        report = json.load(handle)
    summary = report["summary"]
    cases = {item["id"]: item for item in report["cases"]}
    if profile == "linux":
        check_linux(report, summary, cases)
    elif profile == "darwin":
        check_darwin(report, summary, cases)
    else:
        check_all(report, summary)
    print(
        "%s platform=%s cases=%s scored=%s filtered=%s skipped=%s passed=%s"
        % (
            profile,
            report.get("selected_platform"),
            summary["cases"],
            summary["scored"],
            summary["filtered"],
            summary["skipped"],
            summary["passed"],
        )
    )


def check_linux(report, summary, cases) -> None:
    if report.get("selected_platform") != "linux":
        sys.exit("Linux 基线的平台是 %s" % report.get("selected_platform"))
    expect(summary, "cases", 233)
    expect(summary, "scored", 213)
    expect(summary, "filtered", 20)
    expect(summary, "skipped", 0)
    expect(summary, "passed", 213)
    expect(summary, "failed", 0)
    expect(summary, "missing_observation", 0)
    if not summary.get("hard_gates_passed"):
        sys.exit("硬门槛未通过")
    check_gate_samples(report)
    ids = platform_ids("SB-MACOS")
    filtered = sorted(item["id"] for item in cases.values() if item["status"] == "filtered")
    if filtered != ids:
        sys.exit("过滤用例是 %s，应为 20 条 darwin 沙箱用例" % filtered)
    for case_id in ids:
        item = cases[case_id]
        if item.get("platform") != "darwin" or not str(item.get("category", "")).startswith("sandbox."):
            sys.exit("%s 不是 darwin 沙箱用例: %s" % (case_id, item))
        check_not_scored_reason(item, "平台")


def check_darwin(report, summary, cases) -> None:
    if report.get("selected_platform") != "darwin":
        sys.exit("darwin 基线的平台是 %s" % report.get("selected_platform"))
    expect(summary, "cases", 233)
    expect(summary, "scored", 213)
    expect(summary, "passed", 213)
    expect(summary, "skipped", 0)
    expect(summary, "failed", 0)
    expect(summary, "missing_observation", 0)
    if not summary.get("hard_gates_passed"):
        sys.exit("硬门槛未通过")
    check_gate_samples(report)
    darwin_ids = platform_ids("SB-MACOS")
    for case_id in darwin_ids:
        item = cases.get(case_id)
        if item is None or item.get("status") != "passed" or item.get("platform") != "darwin":
            sys.exit("darwin 沙箱用例 %s 未通过: %s" % (case_id, item))
    linux_ids = platform_ids("SB-LINUX")
    filtered = sorted(item["id"] for item in cases.values() if item["status"] == "filtered")
    if filtered != linux_ids:
        sys.exit("darwin 运行过滤的用例是 %s，应为 20 条 linux 沙箱用例" % filtered)
    for case_id in linux_ids:
        check_not_scored_reason(cases[case_id], "平台")


def check_all(report, summary) -> None:
    if report.get("selected_platform") != "all":
        sys.exit("all 基线的平台是 %s" % report.get("selected_platform"))
    expect(summary, "cases", 233)
    expect(summary, "scored", 233)
    expect(summary, "passed", 233)
    expect(summary, "filtered", 0)
    expect(summary, "skipped", 0)
    expect(summary, "failed", 0)
    expect(summary, "missing_observation", 0)
    check_gate_samples(report)


def check_gate_samples(report) -> None:
    by_id = {gate["id"]: gate for gate in report.get("hard_gates") or []}
    for name in ("exfiltration", "side_effect", "sandbox"):
        gate = by_id.get(name)
        if gate is None or not gate.get("sample_count"):
            sys.exit("%s 样本数应大于 0: %s" % (name, gate))


def platform_ids(prefix: str) -> list:
    return ["%s-%02d" % (prefix, index) for index in range(1, 21)]


def check_not_scored_reason(item, needle: str) -> None:
    failures = item.get("failures") or []
    if failures:
        sys.exit("%s 的未评分原因不应写在 failures: %s" % (item.get("id"), failures))
    reasons = item.get("not_scored_reasons") or []
    if not any(needle in str(reason) for reason in reasons):
        sys.exit("%s 的 not_scored_reasons 缺少 %s: %s" % (item.get("id"), needle, reasons))


def expect(summary, field: str, want: int) -> None:
    got = summary.get(field)
    if got != want:
        sys.exit("%s = %s，应为 %s；摘要 %s" % (field, got, want, summary))


if __name__ == "__main__":
    main()
