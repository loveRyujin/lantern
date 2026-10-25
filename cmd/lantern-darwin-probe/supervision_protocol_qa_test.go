package main

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func qaSupervisionFrame() supervisionFrame {
	return supervisionFrame{Kind: "PREPARED", Nonce: strings.Repeat("ab", 20), Seq: 1, PID: 1234, StartSeconds: 1700000000, StartMicroseconds: 42, Image: "/trusted/lantern-probe"}
}

func TestSupervisionQAProtocolIdentityAndFraming(t *testing.T) {
	f := qaSupervisionFrame()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeSupervisionFrame(b, f.Nonce, 1)
	if err != nil || got != f {
		t.Fatalf("valid direct registration rejected: %+v %v", got, err)
	}
	exact := append(append([]byte(nil), b...), []byte(strings.Repeat(" ", 4096-len(b)))...)
	if _, err := decodeSupervisionFrame(exact, f.Nonce, 1); err != nil {
		t.Fatalf("valid exact 4KiB frame rejected: %v", err)
	}
	for name, body := range map[string][]byte{
		"empty": nil, "null": []byte("null"), "trailing": append(append([]byte(nil), b...), []byte(" {}")...),
		"truncated": b[:len(b)-1], "one-byte-over": append(append([]byte(nil), exact...), byte(' ')),
		"unknown-field":   []byte(strings.TrimSuffix(string(b), "}") + `,"unknown":true}`),
		"duplicate-nonce": []byte(`{"nonce":"wrong",` + strings.TrimPrefix(string(b), "{")),
		"invalid-utf8":    []byte(strings.Replace(string(b), "/trusted/lantern-probe", "/trusted/"+string([]byte{255}), 1)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeSupervisionFrame(body, f.Nonce, 1); err == nil {
				t.Fatal("malformed protocol frame accepted")
			}
		})
	}
	unicode := f
	unicode.Image = "/trusted/" + strings.Repeat("图", 1400)
	body, _ := json.Marshal(unicode)
	if len(body) <= 4096 || utf8.RuneCount(body) >= 4096 {
		t.Fatal("invalid byte/rune boundary fixture")
	}
	if _, err := decodeSupervisionFrame(body, f.Nonce, 1); err == nil {
		t.Fatal("4KiB protocol budget measured in runes rather than bytes")
	}
}

func TestSupervisionQAProtocolRejectsStaleIdentity(t *testing.T) {
	for name, change := range map[string]func(*supervisionFrame){
		"nonce": func(f *supervisionFrame) { f.Nonce = "stale" }, "sequence": func(f *supervisionFrame) { f.Seq = 2 },
		"unknown-kind": func(f *supervisionFrame) { f.Kind = "EXEC" }, "zero-pid": func(f *supervisionFrame) { f.PID = 0 },
		"zero-birth": func(f *supervisionFrame) { f.StartSeconds = 0 }, "invalid-usec": func(f *supervisionFrame) { f.StartMicroseconds = 1000000 },
		"empty-image": func(f *supervisionFrame) { f.Image = "" }, "relative-image": func(f *supervisionFrame) { f.Image = "probe" },
		"unclean-image": func(f *supervisionFrame) { f.Image = "/trusted/../probe" },
	} {
		t.Run(name, func(t *testing.T) {
			f := qaSupervisionFrame()
			nonce := f.Nonce
			change(&f)
			if err := validateSupervisionFrame(f, nonce, 1); err == nil {
				t.Fatal("stale/invalid identity accepted")
			}
		})
	}
	f := qaSupervisionFrame()
	f.Seq = 0
	if err := validateSupervisionFrame(f, f.Nonce, 0); err == nil {
		t.Fatal("protocol sequence must start at one")
	}
}
