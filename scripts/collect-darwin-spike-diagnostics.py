#!/usr/bin/env python3
"""Preserve bounded native diagnostics for PIDs owned by this fixed experiment."""
import datetime
import json
import pathlib
import re
import subprocess
import sys

out = pathlib.Path(sys.argv[1])
report_path = out / 'report.json'
if not report_path.is_file():
    raise SystemExit(0)
report = json.loads(report_path.read_text())
pids = set()
for stage in report.get('stages', []):
    for run in stage.get('runs', []):
        for key in ('pid', 'payload_pid'):
            pid = run.get(key)
            if isinstance(pid, int) and pid > 0:
                pids.add(pid)
if not pids:
    raise SystemExit(0)
predicate = ' OR '.join('eventMessage CONTAINS "(%d)"' % pid for pid in sorted(pids))
try:
    logs = subprocess.run(['/usr/bin/log', 'show', '--style', 'json', '--last', '5m', '--predicate', predicate], capture_output=True, timeout=20, check=False)
    (out / 'native-log.json').write_bytes(logs.stdout[:512 * 1024])
    (out / 'native-log-status.json').write_text(json.dumps({'exit_code': logs.returncode, 'original_bytes': len(logs.stdout), 'truncated': len(logs.stdout) > 512 * 1024, 'stderr': logs.stderr.decode(errors='replace')[:4096]}))
except (OSError, subprocess.TimeoutExpired) as exc:
    (out / 'native-log-status.json').write_text(json.dumps({'unavailable': str(exc)}))
# Apple .ips files consist of a metadata JSON line followed by a report object.
# Keep only runtime/termination fields from our PIDs; omit unrelated reports.
started = datetime.datetime.fromisoformat(report['started'].replace('Z', '+00:00')).timestamp()
crashes = []
folder = pathlib.Path.home() / 'Library/Logs/DiagnosticReports'
for path in sorted(folder.glob('*.ips')):
    stat = path.stat()
    if stat.st_mtime < started or stat.st_size > 2 * 1024 * 1024:
        continue
    raw = path.read_text(errors='replace')
    if not any(re.search(r'"pid"\s*:\s*%d\b' % pid, raw) for pid in pids):
        continue
    try:
        _, body = raw.split('\n', 1)
        crash = json.loads(body)
    except (ValueError, json.JSONDecodeError):
        continue
    if crash.get('pid') not in pids:
        continue
    crashes.append({key: crash[key] for key in ('pid', 'procName', 'procPath', 'captureTime', 'translated', 'exception', 'termination', 'asi', 'codeSigningID') if key in crash})
(out / 'owned-crash-summaries.json').write_text(json.dumps(crashes, indent=2))
