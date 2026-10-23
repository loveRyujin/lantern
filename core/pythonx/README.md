# Production analysis tools

Trusted setup explicitly calls `pyrt.Config.Ensure`, then creates one Session for
one workspace and one Agent run. Neither tool installs or repairs Python.

```go
session, err := pythonx.New(ctx, pythonx.Config{
    Workspace: ws, RunID: runID, Runtime: preparedRuntime,
})
// Handle err; keep the session alive until the caller has fetched its figures.
defer session.Close()
registry.Register(session.SQLTool())
registry.Register(session.PythonTool())
```

The existing Registry, Policy and Approver still control execution. The default
profile only accepts `code` and optional `sql_call_id`; model parameters cannot
request networking, runtime paths, arbitrary directories or larger limits.
Production defaults always use the real Linux Runner with workspace-local tmp.

`run_sql` stores successful observations by explicit call ID, scoped to the
Session's workspace/run. Duplicate IDs never replace an old result. The cache
holds at most 32 observations / 8 MiB; in-flight queries reserve their worst-case
1 MiB result until completion. Full caches fail explicitly without selecting a
different observation. Failed SQL produces no usable reference.

For `run_python`, use the actual successful SQL call ID and code such as:

```python
columns, rows, metadata = load_table()
import os, pandas as pd, matplotlib.pyplot as plt
frame = pd.DataFrame(rows, columns=columns, dtype=object)
# This example expects a numeric first column; keep the original frame intact.
plt.bar(range(len(frame)), [float(value) for value in frame.iloc[:, 0]])
plt.savefig(os.path.join(os.environ['OUTPUT_DIR'], 'chart.png'))
```

The fixed loader preserves the #42 JSON-cell CSV contract. Truncation metadata
describes returned rows, never the original total. The tool acquires a validated
read-only prepared-runtime lease across Sandbox.Run. Validation checks the
current marker, interpreter metadata, locked package versions and imports without
pip, repair, fallback or user code. Explicit Ensure cannot replace a leased venv.

Successful Content contains bounded stdout/stderr (8 KiB each, UTF-8, known local
paths redacted), observed usage, normalized configured `limits`, and
`artifacts:[{id,name,kind,bytes}]`. Startup failure does not imply limits were
successfully installed. Artifact
bytes stay outside the model transcript. A caller can retrieve an independent
copy with `GetArtifact(workspaceID, runID, id)` after Agent.Run completes and
before Session.Close. This is ephemeral storage, not W7 desktop persistence.

Retained artifact payload is capped at 64 MiB per Session, in addition to Runner
per-batch limits. A full batch is rejected atomically; old figures are not evicted.
The cap is not a total process heap/RSS quota. Close cancels and waits for queued
and active tools to release leases and clean up, then clears sources and figures.
No new batch may commit after Close begins; all subsequent calls/reads fail.

Sandbox and runtime errors become stable, non-retryable `tools.Error` codes with
generic bounded messages. Raw tracebacks, paths and code are not written into the
error/audit message. `MaxAttempts=1`; resource errors remain execution feedback,
not consecutive parameter-validation failures.
