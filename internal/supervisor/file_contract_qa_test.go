package supervisor

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestFileQACandidateStopIsOneStatement(t *testing.T) {
	b, err := os.ReadFile("resources_darwin.go")
	if err != nil {
		t.Fatal(err)
	}
	const stmt = "if s.Files.Allocated > r.budget.FileBytes {"
	if got := strings.Count(string(b), stmt); got != 1 {
		t.Fatalf("candidate allocated stop must be one mutable statement, found %d", got)
	}
	if strings.Contains(string(b), "if false && s.Files.Allocated > r.budget.FileBytes {") {
		t.Fatal("candidate stop is already disabled in the source under test")
	}
}

func TestFileQAMarkerFrameFitsProtocol(t *testing.T) {
	nonce := strings.Repeat("ab", 16)
	cp := FileCheckpoint{Phase: "grew", Written: FileUnlinkNamed + FileUnlinkExtra, Dev: 99, Ino: 100, Live: true, NLink: 0, Unlinked: true, LogicalNamed: 1, LogicalUnique: 1, Allocated: 1, Entries: 2, UniqueInodes: 1, Symlinks: 0}
	w := WorkerResult{Entered: true, PID: 4242, ExitCode: -1, Mode: FileUnlink, Session: 9, Group: 9, OriginalGroup: 8, File: &cp, EnvKeys: []string{"PATH", "LANG", "LC_ALL", "GOMAXPROCS"}}
	for i := 0; i < 8; i++ {
		w.OpenFDs = append(w.OpenFDs, FDIdentity{Device: uint64(1000 + i), Inode: uint64(2000 + i), Mode: 0100600})
	}
	id := Identity{PID: 4242, StartSeconds: 1700000000, StartMicroseconds: 123456, Image: "/" + strings.Repeat("p", 180), Nonce: nonce}
	msg := message{Version: 1, Kind: "PROGRESS", Seq: 4, Nonce: nonce, Target: id, Worker: &w}
	b, err := json.Marshal(msg)
	if err != nil || len(b) > 4096 {
		t.Fatalf("progress frame does not fit the sealed protocol: %d %v", len(b), err)
	}
	receipt := receipt{Target: id, Worker: w, Waited: true, Absent: true, Clean: true, SignalSent: true}
	done := message{Version: 1, Kind: "COMPLETE", Seq: 5, Nonce: nonce, Target: id, Receipt: &receipt}
	b, err = json.Marshal(done)
	if err != nil || len(b) > 4096 {
		t.Fatalf("completion frame does not fit the sealed protocol: %d %v", len(b), err)
	}
}
