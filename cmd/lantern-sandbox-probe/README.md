# Linux sandbox spike (#40)

This is a fixed experiment, not an implementation of `Sandbox.Run`. It accepts
no user command or user code. Build with `CGO_ENABLED=0`; the Landlock/seccomp
implementation intentionally supports Linux amd64 only. Other platforms produce
an `unsupported` JSON result and exit 2.

```sh
LANTERN_REQUIRE_LANDLOCK=1 LANTERN_REQUIRE_BWRAP=1 \
  scripts/probe-linux-sandbox.sh /tmp/lantern-linux-sandbox-report.json
```

The script builds a temporary static Go executable, runs the fixed probes and
writes a structured JSON report. A failed check exits nonzero. An unsupported
backend is explicitly recorded; setting its `LANTERN_REQUIRE_*` variable makes
unsupported fail CI too. Neither backend falls back to host execution. Python 3
and, for comparison, bubblewrap are external experiment dependencies. The
reported `experiment_elapsed_ns` includes process creation and all checks; it is
not a pure startup benchmark or a performance guarantee.

## Landlock + seccomp

A disposable parent creates input/runtime/out/tmp and a sibling host-secret
fixture. A trusted launcher locks its OS thread, enables `no_new_privs`, installs
Landlock, installs seccomp with thread synchronization and execs the fixed static
probe. That exec replaces existing Go threads; its new runtime threads inherit
the calling thread's restrictions. A real Python exec then verifies inheritance
again. No arbitrary Python source is accepted.

Landlock requires ABI >= 3 and handles the ABI 3 filesystem rights, including
REFER and TRUNCATE. Input is read-only, the runtime is read/execute, and out/tmp
permit ordinary filesystem writes. Newer ABI rights (e.g. device ioctl controls)
are not claimed. The default denies opening the sibling secret and /etc/passwd.

This experiment explicitly permits /usr, /lib, /lib64 as read-only runtime roots
for the host Python interpreter and standard library. That exposes their contents
and is broader than a minimal packaged Python runtime. The production launcher
must choose and validate the actual pyrt dependency closure separately.

Seccomp checks the audit architecture, rejects the amd64 x32 syscall bit, and
returns EPERM for all socket creation families/socketpair and network operations,
io_uring, ptrace, process_vm, pidfd_getfd, BPF and selected namespace/mount escape
surfaces. This is an experimental denylist, **not a reviewed production syscall
allowlist**. Supported ABIs, newly added syscalls and inherited descriptors need
separate production analysis. The probe does not revoke already-open filesystem
descriptors, and Landlock does not generally hide filesystem metadata.

## bubblewrap comparison

The fixed Python probe runs under `bwrap --unshare-all` in a separate mount and
network namespace with read-only input/runtime and writable out/tmp. It checks
that the host-secret path is absent and that a TCP listener proven reachable on
the parent host becomes unreachable. It also verifies that AF_UNIX, INET, INET6
and NETLINK socket creation remains possible: a network namespace alone does
not implement the product's all-socket denial policy. No seccomp profile is
supplied to this comparison.

This experiment does not claim memory/RSS limits, disk quotas, process-count
limits, descendant timeout cleanup, signal isolation, production environment/FD
sanitization or support for arbitrary Python packages. Those remain #41 work.

## Primary references

- [Linux Landlock userspace API](https://docs.kernel.org/userspace-api/landlock.html)
  explains per-thread enforcement, inherited restrictions, ABI-dependent rights,
  filesystem limitations and already-open descriptors.
- [Linux seccomp filter API](https://docs.kernel.org/userspace-api/seccomp_filter.html)
  explains no_new_privs, audit-architecture checks and filter inheritance.
- [seccomp(2)](https://man7.org/linux/man-pages/man2/seccomp.2.html)
  documents TSYNC and the x86-64/x32 ABI caveat.
- [bubblewrap upstream](https://github.com/containers/bubblewrap)
  documents mount/user/network namespace behavior and its policy boundary.
