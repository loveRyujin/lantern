package main

import (
	"errors"
	"testing"
)

func TestSupervisionQAIdentityUnavailableIsNotBirthMismatch(t *testing.T) {
	want := supervisionIdentity{PID: 1234, StartSeconds: 1700000000, StartMicroseconds: 42, Image: "/trusted/probe", Nonce: "fixed"}
	for name, observed := range map[string]supervisionIdentity{"unavailable": {}, "stale-buffer": want} {
		t.Run(name, func(t *testing.T) {
			err := classifySupervisionIdentity(want, observed, errors.New("pidinfo short read"))
			if !errors.Is(err, errSupervisionIdentityUnavailable) || errors.Is(err, errSupervisionBirthMismatch) {
				t.Fatalf("unavailable identity was promoted to a real conflicting birth: %v", err)
			}
		})
	}
	for name, change := range map[string]func(*supervisionIdentity){"pid": func(v *supervisionIdentity) { v.PID++ }, "seconds": func(v *supervisionIdentity) { v.StartSeconds++ }, "microseconds": func(v *supervisionIdentity) { v.StartMicroseconds++ }} {
		t.Run(name, func(t *testing.T) {
			observed := want
			change(&observed)
			err := classifySupervisionIdentity(want, observed, nil)
			if !errors.Is(err, errSupervisionBirthMismatch) || errors.Is(err, errSupervisionIdentityUnavailable) {
				t.Fatalf("real conflicting birth was downgraded to transient unavailability: %v", err)
			}
		})
	}
	observed := want
	observed.Image = "/runtime/Python.app/Contents/MacOS/Python"
	if err := classifySupervisionIdentity(want, observed, nil); err != nil {
		t.Fatalf("registered PID/birth across an allowed exec was rejected: %v", err)
	}
}
