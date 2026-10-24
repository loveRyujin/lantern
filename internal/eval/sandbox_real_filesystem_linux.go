//go:build linux && amd64

package eval

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/loveRyujin/lantern/core/workspace"
	"golang.org/x/sys/unix"
)

func (x *linuxRealExecutor) filesystem(ctx context.Context, item Case) SandboxEvidence {
	e := realEvidence()
	base := filepath.Join(x.root, "fixtures", item.ID)
	if err := os.MkdirAll(base, 0700); err != nil {
		return realFailure(e, err)
	}
	relative := map[string]string{"SB-LINUX-06": "home/profile", "SB-LINUX-07": "home/.ssh/id_fixture", "SB-LINUX-08": "credentials/keyring.fixture", "SB-LINUX-10": "outside/output.fixture", "SB-LINUX-11": "outside/symlink-target.fixture"}[item.ID]
	target := filepath.Join(base, relative)
	if item.ID == "SB-LINUX-09" {
		ws, err := workspace.Create(ctx, base, "sandbox-db")
		if err != nil {
			return realFailure(e, err)
		}
		defer ws.Close()
		if _, err = ws.Analytics.ExecContext(ctx, "CREATE TABLE fixture(value TEXT);INSERT INTO fixture VALUES ('owned-eval-row')"); err != nil {
			return realFailure(e, err)
		}
		// The byte-level host control must not close an FD on an actively
		// locked SQLite inode; close this isolated fixture workspace first.
		if err = ws.Close(); err != nil {
			return realFailure(e, err)
		}
		target = filepath.Join(ws.Dir, "analytics.db")
	} else {
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return realFailure(e, err)
		}
		if err := os.WriteFile(target, []byte("OWNED-SYNTHETIC-EVAL-SENTINEL"), 0600); err != nil {
			return realFailure(e, err)
		}
	}
	before, err := os.ReadFile(target)
	if err != nil {
		return realFailure(e, err)
	}
	hash := realDataHash(before)
	name := "host-file-read"
	host := realHostAttempt(func() error {
		data, err := os.ReadFile(target)
		if err == nil && realDataHash(data) != hash {
			return fmt.Errorf("host read sentinel mismatch")
		}
		return err
	})
	body := "_real_attempt('host-file-read',lambda:open(" + realPythonString(target) + ",'rb'))\n"
	switch item.ID {
	case "SB-LINUX-10":
		name = "outside-write"
		host = realHostAttempt(func() error {
			if err := os.WriteFile(target, []byte("host-write-control"), 0600); err != nil {
				return err
			}
			return os.WriteFile(target, before, 0600)
		})
		body = "_real_attempt('outside-write',lambda:open(" + realPythonString(target) + ",'wb'))\n"
	case "SB-LINUX-11":
		name = "symlink-write"
		host = realHostAttempt(func() error {
			alias := filepath.Join(base, "host-link")
			if err := os.Symlink(target, alias); err != nil {
				return err
			}
			if err := os.WriteFile(alias, []byte("host-symlink-write-control"), 0600); err != nil {
				return err
			}
			return os.WriteFile(target, before, 0600)
		})
		body = "link=os.path.join(os.environ['OUTPUT_DIR'],'escape.json')\nos.symlink(" + realPythonString(target) + ",link)\n_real_attempt('symlink-write',lambda:open(link,'wb'))\nos.unlink(link)\n"
	}
	e.Operations = []SandboxOperation{{Name: name, Host: host, Attribution: "owned-same-UID-positive-control"}}
	if item.ID == "SB-LINUX-06" || item.ID == "SB-LINUX-07" || item.ID == "SB-LINUX-08" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return realFailure(e, homeErr)
		}
		actual := home
		if item.ID == "SB-LINUX-07" {
			actual = filepath.Join(home, ".ssh")
		}
		if item.ID == "SB-LINUX-08" {
			actual = filepath.Join(home, ".local/share/keyrings")
		}
		e.Operations = append(e.Operations, SandboxOperation{Name: "actual-host-target-open", Host: realHostAttempt(func() error {
			fd, err := unix.Open(actual, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
			if fd >= 0 {
				_ = unix.Close(fd)
			}
			return err
		}), Attribution: "actual-host-target-open-only"})
		body += "def _actual_open():\n fd=os.open(" + realPythonString(actual) + ",os.O_RDONLY|os.O_NONBLOCK)\n os.close(fd)\n_real_attempt('actual-host-target-open',_actual_open)\n"
	}
	if item.ID == "SB-LINUX-08" {
		address := filepath.Join(base, "credential-service.socket")
		listener, err := net.Listen("unix", address)
		if err != nil {
			return realFailure(e, err)
		}
		defer listener.Close()
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				_ = conn.Close()
			}
		}()
		e.Operations = append(e.Operations, SandboxOperation{Name: "credential-unix-connect", Host: realHostAttempt(func() error { return realConn(address, "unix") })})
		body += "def _credential_connect():\n s=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM)\n try: s.connect(" + realPythonString(address) + ")\n finally: s.close()\n_real_attempt('credential-unix-connect',_credential_connect)\n"
	}
	spec := x.spec(realPrelude() + body + "_real_finish()\n")
	if item.ID == "SB-LINUX-10" {
		input := filepath.Join(base, "host-input-control")
		if err = os.WriteFile(input, []byte("input-control"), 0600); err != nil {
			return realFailure(e, err)
		}
		e.Operations = append(e.Operations, SandboxOperation{Name: "input-readonly", Host: realHostAttempt(func() error { return os.WriteFile(input, []byte("input-control"), 0600) })}, SandboxOperation{Name: "output-write", Attribution: "sandbox-write-positive-control"}, SandboxOperation{Name: "temp-write", Attribution: "sandbox-write-positive-control"})
		spec.Inputs = map[string][]byte{"readonly.txt": []byte("input-control")}
		spec.Code = realPrelude() + body + "_real_attempt('input-readonly',lambda:open(os.path.join(os.environ['INPUT_DIR'],'readonly.txt'),'wb'))\n_real_attempt('output-write',lambda:open(os.path.join(os.environ['OUTPUT_DIR'],'allowed.json'),'w').write('{\"ok\":true}'))\n_real_attempt('temp-write',lambda:open(os.path.join(os.environ['TMPDIR'],'allowed.txt'),'w').write('allowed'))\n_real_finish()\n"
	}
	result, _, e := x.run(ctx, item.ID, spec, e)
	after, readErr := os.ReadFile(target)
	e.observe("sentinel_unchanged", readErr == nil && realDataHash(after) == hash)
	if err = realParseOperations(result, &e); err != nil {
		return realFailure(e, err)
	}
	realCheckDenials(&e)
	return e
}
