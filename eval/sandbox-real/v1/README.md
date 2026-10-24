# Real Linux sandbox suite v1

This suite is separate from the frozen `eval/v1` Mock baseline. `sandbox-real`
executes fixed, bounded payloads through the production Runner. It ignores
`input.mock`; the versioned fixture ID, exact required rules and legacy lineage
must still match. Unknown, missing or altered cases/rules fail closed.

SB-LINUX-01–20 preserve the original intentions. SB-LINUX-21–25 add cumulative
CPU, parent application death, forced capability failure, production Session PNG
and cancellation. Reports count the two groups independently; passing requires
all 25, with no skipped, filtered or missing observations.

Prepare explicitly (this step may download hash-locked dependencies):

```sh
go run ./cmd/lantern-eval prepare-runtime --root /tmp/lantern-eval-runtime --out /tmp/lantern-eval-runtime.json
```

Execute without installing or repairing:

```sh
go run ./cmd/lantern-eval run --mode sandbox-real --runtime-config /tmp/lantern-eval-runtime.json --out reports/sandbox-real
# Or: make eval-sandbox-real EVAL_RUNTIME_CONFIG=/tmp/lantern-eval-runtime.json
```

Preparation refuses to overwrite a config. Use a new output path when preparing
again. Execution requires a supported Linux amd64, non-root/capability-free user,
Landlock ABI >=3 and the prepared runtime. Missing prerequisites produce a failed
report, not a skipped case or host fallback.

Historical `killed`, `output_limit` and `permission_denied` Mock values are not
fabricated: this profile rejects process creation with actual EPERM, reports the
Runner's real error codes, and records attempts separately from successful
escapes. Network/file host controls and attribution limits are part of evidence.
Raw INET ambient denial is not independent proof of seccomp; bounded NETLINK RAW
and ordinary TCP/UDP probes provide stronger controls. No real credentials are
printed and no unbounded fork bomb or live application ptrace is attempted.

Reports describe fixed-code sandbox evidence, with no cloud LLM or embedding
judge. Whole-project `live`, macOS validation and desktop UI remain separate.
See `docs/adr/0009-real-linux-sandbox-eval.md` for the frozen design and limits.
