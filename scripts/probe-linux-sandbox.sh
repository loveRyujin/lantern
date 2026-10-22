#!/usr/bin/env bash
# Fixed experiments only; never executes supplied user code on the host.
set -euo pipefail
cd "$(dirname "$0")/.."
report=${1:-/tmp/lantern-linux-sandbox-report.json}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
CGO_ENABLED=0 go build -o "$work/probe" ./cmd/lantern-sandbox-probe
python3 scripts/probe-linux-sandbox.py "$work/probe" "$report"
