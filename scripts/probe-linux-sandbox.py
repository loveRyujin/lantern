#!/usr/bin/env python3
"""Fixed bwrap comparison. This script does not accept a user payload."""
import errno
import json
import os
from pathlib import Path
import platform
import shutil
import socket
import stat
import subprocess
import sys
import tempfile
import time


def payload(base, secret, port):
    """Only run inside bwrap; host mode is never offered."""
    observations = []

    def observe(name, action, allowed, denied_errnos=()):
        try:
            action()
            observations.append(dict(name=name, passed=allowed, detail="operation succeeded"))
        except OSError as exc:
            observations.append(dict(name=name, passed=not allowed and exc.errno in denied_errnos,
                                     errno=exc.errno, detail=str(exc)))

    def read(path):
        return Path(path).read_text()

    def write(path):
        Path(path).write_text("probe")

    def create_socket(family, kind=socket.SOCK_STREAM):
        with socket.socket(family, kind):
            pass

    def connect():
        with socket.socket() as sock:
            sock.settimeout(1)
            sock.connect(("127.0.0.1", int(port)))

    deny = (errno.EACCES, errno.EPERM, errno.EROFS, errno.ENOENT)
    for directory in ("input", "runtime"):
        observe("read-" + directory, lambda d=directory: read(base + "/" + d + "/data"), True)
        observe("deny-write-" + directory,
                lambda d=directory: write(base + "/" + d + "/data"), False, deny)
    observe("host-secret-hidden", lambda: read(secret), False, deny)
    for directory in ("out", "tmp"):
        observe("write-" + directory, lambda d=directory: write(base + "/" + d + "/created"), True)
    # A network namespace isolates connectivity, but does not prohibit sockets.
    for family in (socket.AF_INET, socket.AF_INET6, socket.AF_UNIX):
        observe("socket-created-family-" + str(family), lambda f=family: create_socket(f), True)
    observe("socket-created-netlink", lambda: create_socket(socket.AF_NETLINK, socket.SOCK_RAW), True)
    observe("host-listener-unreachable", connect, False,
            (errno.ECONNREFUSED, errno.ENETUNREACH, errno.EHOSTUNREACH, errno.EPERM))
    print(json.dumps(dict(checks=observations)))


def run_bwrap():
    result = dict(backend="bwrap", status="unsupported")
    binary = shutil.which("bwrap")
    if not binary:
        result["error"] = "bwrap not installed; no payload executed"
        return result
    result["binary"] = binary
    result["setuid"] = bool(os.stat(binary).st_mode & stat.S_ISUID)
    result["version"] = subprocess.run([binary, "--version"], capture_output=True,
                                       text=True, timeout=5).stdout.strip()
    with tempfile.TemporaryDirectory(prefix="lantern-bwrap-probe-") as directory:
        root = Path(directory)
        for part in ("input", "runtime", "out", "tmp"):
            (root / part).mkdir()
        for part in ("input/data", "runtime/data", "host-secret"):
            (root / part).write_text("fixture")
        controls = []
        for part in ("input/data", "runtime/data", "host-secret"):
            path = root / part
            original = path.read_text()
            path.write_text("control-write")
            assert path.read_text() == "control-write"
            path.write_text(original)
            controls.append(dict(name="host-read-write-restore-" + part, passed=True))
        result["controls"] = controls
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            listener.listen(1)
            # Prove the host endpoint exists before probing its network namespace.
            with socket.create_connection(listener.getsockname(), timeout=1):
                pass
            accepted, _ = listener.accept()
            accepted.close()
            result["controls"].append(dict(name="host-listener-reachable", passed=True))
            command = [binary, "--unshare-all", "--die-with-parent", "--new-session",
                       "--ro-bind", "/usr", "/usr"]
            for path in ("/lib", "/lib64", "/bin"):
                if Path(path).is_symlink():
                    command += ["--symlink", os.readlink(path), path]
                elif Path(path).exists():
                    command += ["--ro-bind", path, path]
            command += ["--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
                        "--ro-bind", str(root / "input"), "/probe/input",
                        "--ro-bind", str(root / "runtime"), "/probe/runtime",
                        "--bind", str(root / "out"), "/probe/out",
                        "--bind", str(root / "tmp"), "/probe/tmp",
                        "--ro-bind", str(Path(__file__).resolve()), "/probe/fixed-probe.py",
                        "--clearenv", "--setenv", "PATH", "/usr/bin",
                        "/usr/bin/python3", "-I", "/probe/fixed-probe.py", "--inside-bwrap",
                        "/probe", str(root / "host-secret"), str(listener.getsockname()[1])]
            started = time.monotonic_ns()
            try:
                process = subprocess.run(command, capture_output=True, text=True, timeout=15)
            except subprocess.TimeoutExpired:
                return dict(backend="bwrap", status="failed", error="fixed payload timeout")
            result["experiment_elapsed_ns"] = time.monotonic_ns() - started
            if process.returncode:
                result["error"] = process.stderr[:2000]
                # Namespace/kernel setup failure: the fixed payload never ran.
                if not process.stderr.startswith("bwrap:"):
                    result["status"] = "failed"
                return result
            result.update(json.loads(process.stdout))
            result["status"] = "passed" if all(c["passed"] for c in result["checks"]) else "failed"
    return result


def main():
    if len(sys.argv) == 5 and sys.argv[1] == "--inside-bwrap":
        payload(*sys.argv[2:])
        return
    if len(sys.argv) != 3:
        raise SystemExit("usage: probe-linux-sandbox.py FIXED_GO_PROBE REPORT_JSON")
    started = time.monotonic_ns()
    process = subprocess.run([sys.argv[1]], capture_output=True, text=True, timeout=30)
    try:
        landlock = json.loads(process.stdout)
    except ValueError:
        landlock = dict(backend="landlock-seccomp", status="failed", error=process.stderr[:2000])
    if process.returncode != 0 and landlock.get("status") == "passed":
        landlock.update(status="failed", error="probe emitted passed but exited nonzero")
    landlock["experiment_elapsed_ns"] = time.monotonic_ns() - started
    bwrap = run_bwrap()
    report = dict(schema_version=1, kernel=platform.release(), architecture=platform.machine(),
                  environment=dict(uid=os.getuid(), gid=os.getgid()),
                  purpose="fixed spike only; no Sandbox.Run, resource limits, or user code API",
                  experiments=[landlock, bwrap])
    Path(sys.argv[2]).write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, indent=2))
    failed = any(r["status"] == "failed" for r in report["experiments"])
    for name, result in (("LANDLOCK", landlock), ("BWRAP", bwrap)):
        if os.getenv("LANTERN_REQUIRE_" + name) == "1" and result["status"] != "passed":
            failed = True
    raise SystemExit(1 if failed else 0)


if __name__ == "__main__":
    main()
