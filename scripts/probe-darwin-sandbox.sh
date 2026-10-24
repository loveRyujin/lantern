#!/bin/sh
# Strict fixed-payload macOS spike. Every outcome retains a structured report.
set -eu
probe=
runtime_config=
out=
require_feasible=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --probe|--runtime-config|--out)
      key=$1
      [ "$#" -ge 2 ] || { echo "missing value: $key" >&2; exit 1; }
      case "$key" in
        --probe) probe=$2 ;;
        --runtime-config) runtime_config=$2 ;;
        --out) out=$2 ;;
      esac
      shift 2 ;;
    --require-feasible) require_feasible=--require-feasible; shift ;;
    *) echo "unknown option: $1" >&2; exit 1 ;;
  esac
done
[ -n "$probe" ] && [ -n "$runtime_config" ] && [ -n "$out" ] || { echo 'require --probe PATH --runtime-config FILE --out DIR' >&2; exit 1; }
mkdir -p "$out"
set +e
if [ -n "$require_feasible" ]; then
  "$probe" --runtime-config "$runtime_config" --out "$out" --require-feasible > "$out/stdout.json"
else
  "$probe" --runtime-config "$runtime_config" --out "$out" > "$out/stdout.json"
fi
code=$?
set -e
if [ -n "${GITHUB_STEP_SUMMARY:-}" ] && [ -f "$out/report.md" ]; then
  cat "$out/report.md" >> "$GITHUB_STEP_SUMMARY"
fi
exit "$code"
