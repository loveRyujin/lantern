package supervisor

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func qaSupervisorMessage(t *testing.T, kind string) message {
	t.Helper()
	id := Identity{PID: 12345, StartSeconds: 1700000000, StartMicroseconds: 321, Image: filepath.Join(t.TempDir(), "fixed-helper"), Nonce: strings.Repeat("a", 32)}
	m := message{Version: 1, Kind: kind, Seq: 1, Nonce: id.Nonce, Target: id}
	if kind == "ENTERED" {
		m.Worker = &WorkerResult{Entered: true, PID: id.PID, Mode: Normal}
	}
	if kind == "COMPLETE" {
		m.Receipt = &receipt{Target: id, Waited: true, Absent: true, Clean: true}
	}
	return m
}

func qaSupervisorEncode(t *testing.T, m message) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSupervisorQAActualCodecAcceptsFixedMessagesAndExactByteBudget(t *testing.T) {
	for _, kind := range []string{"BOOTSTRAP", "PREPARED", "REGISTERED", "ARMED", "GO", "CANCEL", "ENTERED", "COMPLETE"} {
		t.Run(kind, func(t *testing.T) {
			m := qaSupervisorMessage(t, kind)
			b := qaSupervisorEncode(t, m)
			if _, err := decodeMessage(b, m.Nonce, 1); err != nil {
				t.Fatal("actual message codec rejected its positive control: ", err)
			}
			b = append(b, []byte(strings.Repeat(" ", 4096-len(b)))...)
			if _, err := decodeMessage(b, m.Nonce, 1); err != nil {
				t.Fatal("exact 4KiB message was rejected: ", err)
			}
			if _, err := decodeMessage(append(b, ' '), m.Nonce, 1); err == nil {
				t.Fatal("actual codec accepted 4097 bytes")
			}
		})
	}
	// No launcher exists when a guardian completes the before-spawn abort.
	m := qaSupervisorMessage(t, "COMPLETE")
	m.Target = Identity{Nonce: m.Nonce}
	m.Receipt.Target = m.Target
	m.Receipt.Waited = false
	if _, err := decodeMessage(qaSupervisorEncode(t, m), m.Nonce, 1); err != nil {
		t.Fatal("real zero-task startup completion was rejected: ", err)
	}
}

func TestSupervisorQAActualCodecRejectsEnvelopeAndIdentityContradictions(t *testing.T) {
	for name, mutate := range map[string]func(*message){
		"missing-version":      func(m *message) { m.Version = 0 },
		"wrong-version":        func(m *message) { m.Version = 2 },
		"unknown-kind":         func(m *message) { m.Kind = "EXEC" },
		"stale-nonce":          func(m *message) { m.Nonce = "stale" },
		"stale-identity-job":   func(m *message) { m.Target.Nonce = "other-run" },
		"stale-sequence":       func(m *message) { m.Seq = 2 },
		"missing-sequence":     func(m *message) { m.Seq = 0 },
		"missing-pid":          func(m *message) { m.Target.PID = 0 },
		"missing-birth":        func(m *message) { m.Target.StartSeconds = 0 },
		"invalid-birth-usec":   func(m *message) { m.Target.StartMicroseconds = 1000000 },
		"relative-image":       func(m *message) { m.Target.Image = "relative" },
		"unclean-image":        func(m *message) { m.Target.Image += string(filepath.Separator) + "." },
		"entered-missing-data": func(m *message) { m.Kind = "ENTERED" },
		"complete-no-receipt":  func(m *message) { m.Kind = "COMPLETE" },
	} {
		t.Run(name, func(t *testing.T) {
			m := qaSupervisorMessage(t, "PREPARED")
			nonce := m.Nonce
			mutate(&m)
			if _, err := decodeMessage(qaSupervisorEncode(t, m), nonce, 1); err == nil {
				t.Fatal("invalid actual protocol message was accepted")
			}
		})
	}
	for _, kind := range []string{"ENTERED", "COMPLETE"} {
		m := qaSupervisorMessage(t, kind)
		if kind == "ENTERED" {
			m.Worker.PID++
		} else {
			m.Receipt.Target.PID++
		}
		if _, err := decodeMessage(qaSupervisorEncode(t, m), m.Nonce, 1); err == nil {
			t.Fatal("worker/receipt identity differed from actual message target but passed")
		}
	}
	for _, sequence := range []int{0, 2} {
		m := qaSupervisorMessage(t, "PREPARED")
		if _, err := decodeMessage(qaSupervisorEncode(t, m), m.Nonce, sequence); err == nil {
			t.Fatal("replayed message was accepted at a different expected sequence")
		}
	}
}

func TestSupervisorQAActualCodecRejectsMalformedAndNestedDuplicateJSON(t *testing.T) {
	m := qaSupervisorMessage(t, "PREPARED")
	b := string(qaSupervisorEncode(t, m))
	inputs := map[string][]byte{
		"empty":             nil,
		"null":              []byte("null"),
		"array":             []byte("[" + b + "]"),
		"truncated":         []byte(b[:len(b)-1]),
		"second-document":   []byte(b + b),
		"trailing-garbage":  []byte(b + " !"),
		"unknown-key":       []byte(strings.Replace(b, "{", `{"command":"/bin/sh",`, 1)),
		"duplicate-nonce":   []byte(strings.Replace(b, "{", `{"nonce":"old-job",`, 1)),
		"escaped-duplicate": []byte(strings.Replace(b, "{", `{"no\u006ece":"old-job",`, 1)),
		"nested-duplicate":  []byte(strings.Replace(b, `"target":{`, `"target":{"pid":1,`, 1)),
		"nested-unknown":    []byte(strings.Replace(b, `"target":{`, `"target":{"command":"/bin/sh",`, 1)),
		"invalid-utf8":      append(append([]byte(nil), []byte(b[:len(b)-1])...), 0xff, '}'),
	}
	for name, raw := range inputs {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeMessage(raw, m.Nonce, 1); err == nil {
				t.Fatal("malformed message reached protocol state handling")
			}
		})
	}
	m.Target.Image = string(filepath.Separator) + strings.Repeat("界", 1400)
	long := qaSupervisorEncode(t, m)
	if len(long) <= 4096 {
		t.Fatal("invalid UTF8 byte-budget control")
	}
	if _, err := decodeMessage(long, m.Nonce, 1); err == nil {
		t.Fatal("protocol used rune count rather than actual bytes")
	}
}

func TestSupervisorQAIdentityReadFailureDoesNotBecomeConflictingBirth(t *testing.T) {
	want := Identity{PID: 1234, StartSeconds: 1700000000, StartMicroseconds: 321, Image: "/owned/fixed-helper", Nonce: "owned-run"}
	if err := classifyIdentity(want, want, errors.New("proc_pidinfo unavailable")); !errors.Is(err, errIdentityUnavailable) || errors.Is(err, errBirthMismatch) {
		t.Fatalf("failed identity read used a stale successful-looking buffer: %v", err)
	}
	for _, field := range []string{"pid", "seconds", "usec"} {
		actual := want
		switch field {
		case "pid":
			actual.PID++
		case "seconds":
			actual.StartSeconds++
		case "usec":
			actual.StartMicroseconds++
		}
		if err := classifyIdentity(want, actual, nil); !errors.Is(err, errBirthMismatch) || errors.Is(err, errIdentityUnavailable) {
			t.Fatalf("actual conflicting birth was downgraded to a recoverable short read: %v", err)
		}
	}
}
