"""Repository-fixed, bounded experiments. No code/command comes from config."""
import errno
import json
import os
import sys
import time

with open(sys.argv[1], encoding="utf-8") as handle:
    cfg = json.load(handle)
mode = cfg["mode"]
assert mode in {"filesystem", "network", "process-profile", "setpgid-change", "benign", "cpu", "rss", "vmem", "output", "stderr", "disk", "wall", "sleep", "error", "fontcache"}

def emit(value):
    print(json.dumps(value, ensure_ascii=True), flush=True)

emit({"entered": True, "pid": os.getpid(), "mode": mode})
with open(os.path.join(cfg["out"], "ready.json"), "w", encoding="utf-8") as ready:
    json.dump({"pid": os.getpid(), "mode": mode}, ready)

def operation(name, fn):
    try:
        result = fn()
        emit({"operation": name, "attempted": True, "success": True, "errno": 0,
              "detail": str(result)[:240] if result is not None else ""})
    except (OSError, RuntimeError) as exc:
        emit({"operation": name, "attempted": True, "success": False,
              "errno": getattr(exc, "errno", 0) or 0, "detail": type(exc).__name__})

def read(path):
    with open(path, "rb") as handle:
        assert handle.read() == b"lantern-owned-sentinel\n"

def write(path):
    with open(path, "ab") as handle:
        handle.write(b"x")

def read_database(path):
    with open(path, "rb") as handle:
        assert handle.read(16) == b"SQLite format 3\x00"

if mode == "filesystem":
    operation("input_read", lambda: read(cfg["input"]))
    operation("host_read", lambda: read(cfg["outside"]))
    operation("database_read", lambda: read_database(cfg["database"]))
    operation("symlink_read", lambda: read(os.path.join(os.path.dirname(cfg["input"]), "escape-link")))
    operation("sibling_read", lambda: read(os.path.join(os.path.dirname(cfg["input"]) + "-sibling", "secret")))
    operation("runtime_read", lambda: read(cfg["runtime_sentinel"]))
    operation("input_write", lambda: write(cfg["input"]))
    operation("host_write", lambda: write(cfg["outside"]))
    operation("runtime_write", lambda: write(cfg["runtime_sentinel"]))
    operation("out_write", lambda: write(os.path.join(cfg["out"], "unicode 空格.csv")))
    operation("tmp_write", lambda: write(os.path.join(cfg["tmp"], "temp.bin")))
    # Open/close only: never read or print real credentials.
    for number, path in enumerate(cfg.get("sensitive_targets", [])):
        def open_only(path=path):
            descriptor = os.open(path, os.O_RDONLY | os.O_NONBLOCK)
            os.close(descriptor)
        operation("actual_target_%d" % number, open_only)
elif mode == "network":
    import socket
    def tcp():
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
            s.settimeout(0.5)
            s.connect(("127.0.0.1", cfg["tcp_port"]))
            s.sendall(b"lantern-tcp")
            assert s.recv(32) == b"lantern-ack"
    def udp():
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as s:
            s.settimeout(0.5)
            s.sendto(b"lantern-udp", ("127.0.0.1", cfg["udp_port"]))
            assert s.recv(32) == b"lantern-ack"
    def unix():
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
            s.settimeout(0.5)
            s.connect(cfg["unix_socket"])
            s.sendall(b"lantern-unix")
            assert s.recv(32) == b"lantern-ack"
    operation("tcp_connect", tcp)
    operation("udp_exchange", udp)
    operation("unix_connect", unix)
elif mode == "process-profile":
    import threading
    import signal
    import resource
    def fork_once():
        child = os.fork()
        if child == 0:
            os._exit(0)
        assert os.waitpid(child, 0)[0] == child
    def spawn_once():
        child = os.posix_spawn("/usr/bin/true", ["/usr/bin/true"], {})
        assert os.waitpid(child, 0)[1] == 0
    def thread_once():
        values = []
        thread = threading.Thread(target=lambda: values.append(True))
        thread.start()
        thread.join(1)
        assert values == [True]
    operation("fork", fork_once)
    operation("posix_spawn", spawn_once)
    operation("thread", thread_once)
    operation("cross_signal", lambda: os.kill(cfg["signal_pid"], signal.SIGUSR1))
    original_nproc = resource.getrlimit(resource.RLIMIT_NPROC)
    emit({"operation": "nproc_original_limits", "attempted": True, "success": True,
          "errno": 0, "detail": json.dumps({"uid": os.getuid(), "soft": original_nproc[0], "hard": original_nproc[1]})})
    def nproc_once():
        # Affects this disposable process's soft limit only. Never modify the
        # hard limit or a host process. At most one child, immediately reaped.
        try:
            resource.setrlimit(resource.RLIMIT_NPROC, (0, original_nproc[1]))
            fork_once()
        finally:
            resource.setrlimit(resource.RLIMIT_NPROC, original_nproc)
    operation("nproc_soft_zero_fork", nproc_once)
    # Last: changing session/group must not affect subsequent probes.
    operation("setsid", os.setsid)
elif mode == "setpgid-change":
    original_group = os.getpgrp()
    def change_group():
        assert original_group != os.getpid(), "harness must not make payload a group leader"
        os.setpgid(0, 0)
        assert os.getpgrp() == os.getpid() and os.getpgrp() != original_group
    operation("setpgid", change_group)
elif mode == "benign":
    import pandas as pd
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    frame = pd.DataFrame({"x": [1, 2, 3], "y": [2, 4, 6]})
    assert int(frame["y"].sum()) == 12
    figure, axes = plt.subplots(figsize=(2, 2), dpi=60)
    axes.plot(frame["x"], frame["y"])
    figure.savefig(os.path.join(cfg["out"], "plot.png"))
    plt.close(figure)
    emit({"operation": "pandas_agg", "attempted": True, "success": True, "errno": 0})
elif mode == "fontcache":
    from pathlib import Path
    import matplotlib as mpl
    import matplotlib.font_manager as fm
    bundle = Path(mpl.get_data_path()).resolve()
    fm.fontManager.ttflist = [entry for entry in fm.fontManager.ttflist if Path(entry.fname).resolve().is_relative_to(bundle)]
    fm.fontManager.afmlist = [entry for entry in fm.fontManager.afmlist if Path(entry.fname).resolve().is_relative_to(bundle)]
    assert fm.fontManager.ttflist
    cache = Path(mpl.get_cachedir()) / ("fontlist-v%s.json" % fm.FontManager.__version__)
    fm.json_dump(fm.fontManager, cache)
elif mode == "cpu":
    until = time.monotonic() + 3
    while time.monotonic() < until:
        sum(range(4000))
elif mode == "rss":
    data = bytearray(96 << 20)
    for index in range(0, len(data), 4096):
        data[index] = 1
    time.sleep(1)
elif mode == "vmem":
    import mmap
    mapping = mmap.mmap(-1, 256 << 20)
    assert len(mapping) == 256 << 20
    emit({"operation": "untouched_mapping", "attempted": True, "success": True,
          "errno": 0, "metrics": {"mapped_bytes": len(mapping), "payload_pages_touched": 0},
          "detail": "Actual mmap object length; payload never indexes or writes the mapping. RSS is independently sampled after the payload marker."})
    time.sleep(0.2)
    mapping.close()
elif mode in {"output", "stderr"}:
    stream = sys.stdout if mode == "output" else sys.stderr
    stream.write("x" * (2 << 20))
    stream.flush()
elif mode == "disk":
    with open(os.path.join(cfg["out"], "bounded.bin"), "wb") as handle:
        for unused in range(16):
            handle.write(b"x" * (512 << 10))
            handle.flush()
            time.sleep(0.01)
    time.sleep(0.3)
elif mode in {"wall", "sleep"}:
    time.sleep(5)
elif mode == "error":
    raise RuntimeError("fixed expected failure")

emit({"completed": True, "mode": mode})
