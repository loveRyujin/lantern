//go:build linux || darwin

package sqlx

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestParseDarwinProcArgs(t *testing.T) {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.NativeEndian, uint32(2))
	buf.WriteString("/usr/libexec/lantern-test")
	buf.WriteByte(0)
	buf.Write([]byte{0, 0, 0})
	buf.WriteString("/usr/libexec/lantern-test")
	buf.WriteByte(0)
	buf.WriteString(workerArg)
	buf.WriteByte(0)
	buf.WriteByte(0)
	buf.WriteString("GOMAXPROCS=2")
	buf.WriteByte(0)
	buf.WriteString("GOMEMLIMIT=64MiB")
	buf.WriteByte(0)
	buf.WriteString("TMPDIR=/tmp")
	buf.WriteByte(0)
	buf.WriteByte(0)
	buf.WriteString("OPENAI_API_KEY=sk-darwin-sql-must-not-inherit")
	buf.WriteByte(0)

	argv, env, err := parseDarwinProcArgs(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) != 2 || argv[1] != workerArg || !strings.Contains(strings.Join(argv, "\x00"), workerArg) {
		t.Fatalf("argv = %#v", argv)
	}
	if len(env) != 3 || env[0] != "GOMAXPROCS=2" || env[1] != "GOMEMLIMIT=64MiB" || env[2] != "TMPDIR=/tmp" {
		t.Fatalf("env = %#v", env)
	}

	short := buf.Bytes()[:8]
	if _, _, err := parseDarwinProcArgs(short); err == nil {
		t.Fatal("truncated procargs was accepted")
	}
}
