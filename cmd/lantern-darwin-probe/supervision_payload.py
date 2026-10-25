"""Fixed bounded single-fault payload; no model code or command arguments."""
import json
import os
import sys
import time

with open(sys.argv[1], encoding="utf-8") as handle:
    cfg = json.load(handle)
task = cfg["task"]
mode = cfg["mode"]
assert mode in {"sleep", "normal", "error", "race", "setsid", "setpgid"}
fds = []
for fd in range(3, 128):
    try:
        os.fstat(fd)
        fds.append(fd)
    except OSError:
        pass
stdin_empty = os.read(0, 1) == b""
def fork_probe():
    try:
        pid = os.fork()
        if pid == 0:
            os._exit(0)
        os.waitpid(pid, 0)
        return 0
    except OSError as exc:
        return exc.errno
def spawn_probe():
    try:
        pid = os.posix_spawn("/usr/bin/true", ["/usr/bin/true"], {})
        os.waitpid(pid, 0)
        return 0
    except OSError as exc:
        return exc.errno
fork_errno = fork_probe()
spawn_errno = spawn_probe()
original_group = os.getpgrp()
detached = False
if mode == "setsid":
    os.setsid()
    detached = os.getsid(0) == os.getpid() and os.getpgrp() != original_group
elif mode == "setpgid":
    os.setpgid(0, 0)
    detached = os.getpgrp() == os.getpid() and os.getpgrp() != original_group
marker = {"pid": os.getpid(), "mode": mode, "open_fds": fds, "stdin_empty": stdin_empty,
          "fork_errno": fork_errno, "spawn_errno": spawn_errno, "detached": detached,
          "original_group": original_group, "group": os.getpgrp(), "session": os.getsid(0)}
with open(os.path.join(task["out"], "marker.json"), "w", encoding="utf-8") as handle:
    json.dump(marker, handle)
if mode == "error":
    raise RuntimeError("fixed expected supervision error")
if mode == "normal":
    time.sleep(0.05)
elif mode == "race":
    with open(os.path.join(task["out"], "exiting"), "w", encoding="utf-8") as handle:
        handle.write("exiting")
    deadline = time.monotonic() + 2
    release = os.path.join(task["out"], "exit-release")
    while not os.path.exists(release):
        if time.monotonic() >= deadline:
            raise RuntimeError("cancel/exit rendezvous did not release")
        time.sleep(0.001)
else:
    time.sleep(4)
