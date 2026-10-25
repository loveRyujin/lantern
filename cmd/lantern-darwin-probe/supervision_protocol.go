package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const supervisionProtocol = "v1: guardian owns launcher; launcher registers directly with app; PREPARED -> app registered -> guardian armed -> app GO -> exec; all control FDs CLOEXEC; one fault only; no harness rescue"

var errSupervisionIdentityUnavailable = errors.New("registered identity unavailable; no signal sent")
var errSupervisionBirthMismatch = errors.New("different registered birth; no signal sent")

func classifySupervisionIdentity(expected, observed supervisionIdentity, readErr error) error {
	if readErr != nil {
		return fmt.Errorf("%w: %v", errSupervisionIdentityUnavailable, readErr)
	}
	if observed.PID <= 0 || observed.StartSeconds == 0 || observed.StartMicroseconds >= 1000000 {
		return fmt.Errorf("%w: invalid or incomplete identity fields", errSupervisionIdentityUnavailable)
	}
	if expected.PID <= 0 || observed.PID != expected.PID || observed.StartSeconds != expected.StartSeconds || observed.StartMicroseconds != expected.StartMicroseconds {
		return fmt.Errorf("%w: expected %d/%d.%06d observed %d/%d.%06d", errSupervisionBirthMismatch, expected.PID, expected.StartSeconds, expected.StartMicroseconds, observed.PID, observed.StartSeconds, observed.StartMicroseconds)
	}
	return nil
}

type supervisionFrame struct {
	Kind              string `json:"kind"`
	Nonce             string `json:"nonce"`
	Seq               int    `json:"seq"`
	PID               int    `json:"pid"`
	StartSeconds      uint64 `json:"start_seconds"`
	StartMicroseconds uint64 `json:"start_microseconds"`
	Image             string `json:"image"`
}

func validateSupervisionFrame(f supervisionFrame, nonce string, seq int) error {
	if f.Nonce != nonce || nonce == "" || f.Seq != seq || seq <= 0 || f.Seq <= 0 {
		return fmt.Errorf("protocol nonce/sequence mismatch")
	}
	switch f.Kind {
	case "PREPARED", "REGISTERED", "ARMED", "GO", "CANCEL", "COMPLETE":
	default:
		return fmt.Errorf("unknown protocol kind")
	}
	if f.PID <= 0 || f.StartSeconds == 0 || f.StartMicroseconds >= 1000000 || !filepath.IsAbs(f.Image) || filepath.Clean(f.Image) != f.Image {
		return fmt.Errorf("missing process identity")
	}
	return nil
}
func decodeSupervisionFrame(b []byte, nonce string, seq int) (supervisionFrame, error) {
	var f supervisionFrame
	if len(b) == 0 || len(b) > 4096 || !utf8.Valid(b) {
		return f, fmt.Errorf("protocol frame exceeds 4KiB, is empty or invalid UTF8")
	}
	keys := json.NewDecoder(bytes.NewReader(b))
	token, e := keys.Token()
	if e != nil || token != json.Delim('{') {
		return f, fmt.Errorf("protocol object required")
	}
	seen := map[string]bool{}
	for keys.More() {
		k, e := keys.Token()
		if e != nil {
			return f, e
		}
		name, ok := k.(string)
		if !ok || seen[name] {
			return f, fmt.Errorf("duplicate protocol key")
		}
		seen[name] = true
		var value json.RawMessage
		if e = keys.Decode(&value); e != nil {
			return f, e
		}
	}
	if _, e = keys.Token(); e != nil {
		return f, e
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e := d.Decode(&f); e != nil {
		return f, e
	}
	if e := d.Decode(new(interface{})); e != io.EOF {
		return f, fmt.Errorf("protocol frame must be one complete JSON value")
	}
	return f, validateSupervisionFrame(f, nonce, seq)
}

func supervisionSpec(id string) (window, fault, mode string) {
	mode = "sleep"
	for _, who := range []string{"app", "guardian"} {
		if strings.HasSuffix(id, "-"+who) {
			fault = who
			window = strings.TrimSuffix(id, "-"+who)
			window = strings.TrimPrefix(window, "startup-")
			if window == "setsid" || window == "setpgid" {
				mode = window
			}
			return
		}
	}
	window = id
	switch id {
	case "normal":
		mode = "normal"
	case "error":
		mode = "error"
	case "exit-cancel-race":
		mode = "race"
	}
	return
}

func validateSupervisionCase(c SupervisionCase) error {
	w, f, _ := supervisionSpec(c.ID)
	if c.Window != w || c.FaultTarget != f {
		return fmt.Errorf("case/window/fault manifest mismatch")
	}
	if len(c.Events) == 0 || c.AppPID <= 0 || c.GuardianPID <= 0 || c.AppPID == c.GuardianPID || math.IsNaN(c.WallMS) || math.IsInf(c.WallMS, 0) || math.IsNaN(c.ResponseMS) || math.IsInf(c.ResponseMS, 0) || c.WallMS < 0 || c.WallMS > 8000 || c.ResponseMS < 0 || c.ResponseMS > 2000 {
		return fmt.Errorf("missing processes/events or exceeded deadline")
	}
	if c.GoAuthorized && (!c.Registered || !c.Armed) {
		return fmt.Errorf("GO without registered and armed")
	}
	startup := c.Window == "before-spawn" || c.Window == "after-spawn" || c.Window == "registered" || c.Window == "armed"
	if startup && (c.GoAuthorized || c.PayloadEntered || c.Payload != nil) {
		return fmt.Errorf("startup fault entered payload")
	}
	if !startup && (!c.GoAuthorized || !c.PayloadEntered || c.Payload == nil || c.PayloadPID <= 0) {
		return fmt.Errorf("post-GO payload not observed")
	}
	if c.Payload != nil {
		_, _, mode := supervisionSpec(c.ID)
		if c.Payload.PID != c.PayloadPID || c.PayloadPID != c.LauncherPID || c.Payload.Mode != mode || len(c.Payload.OpenFDs) != 0 || !c.Payload.StdinEmpty || c.Payload.ForkErrno != 1 || c.Payload.SpawnErrno != 1 {
			return fmt.Errorf("payload FD/stdin/no-fork contract failed")
		}
		if (c.Window == "setsid" || c.Window == "setpgid") && !c.Payload.Detached {
			return fmt.Errorf("real session/group change missing")
		}
		if (c.Window == "setsid" || c.Window == "setpgid") && (c.Payload.Group != c.PayloadPID || c.Payload.OriginalGroup == c.Payload.Group) {
			return fmt.Errorf("group change was a no-op")
		}
		if c.Window == "setsid" && c.Payload.Session != c.PayloadPID {
			return fmt.Errorf("session change missing")
		}
	}
	seq := map[string]int{}
	kinds := map[string]bool{}
	positions := map[string]int{}
	for index, e := range c.Events {
		switch e.Actor {
		case "app", "guardian", "launcher", "harness":
		default:
			return fmt.Errorf("unknown event actor")
		}
		expected := map[string]int{"app": c.AppPID, "guardian": c.GuardianPID, "launcher": c.LauncherPID}
		if e.Actor != "harness" && e.PID != expected[e.Actor] {
			return fmt.Errorf("event actor PID mismatch")
		}
		switch e.Kind {
		case "app_started", "guardian_started", "launcher_spawned", "prepared", "registered", "armed", "go_authorized", "go_received", "payload_entered", "fault_window", "fault_injected", "cancel_requested", "target_kill", "signal_skipped", "target_waited", "target_absent", "cleanup", "complete", "peer_lost", "abort":
		default:
			return fmt.Errorf("unknown event kind")
		}
		if e.Kind == "guardian_started" && e.TargetPID != c.GuardianPID {
			return fmt.Errorf("guardian target mismatch")
		}
		if e.Kind == "launcher_spawned" && e.TargetPID != c.LauncherPID {
			return fmt.Errorf("launcher target mismatch")
		}
		switch e.Kind {
		case "prepared", "registered", "armed", "go_authorized", "go_received", "payload_entered":
			if e.TargetPID != c.LauncherPID {
				return fmt.Errorf("protocol target PID mismatch")
			}
		}
		if e.Kind == "target_waited" {
			want := c.LauncherPID
			if e.Actor == "app" {
				want = c.GuardianPID
			}
			if e.TargetPID != want {
				return fmt.Errorf("Wait event describes non-child")
			}
		}
		seq[e.Actor]++
		if e.Seq != seq[e.Actor] || e.AtNS <= 0 || e.PID <= 0 {
			return fmt.Errorf("event order/identity invalid")
		}
		kinds[e.Kind] = true
		key := e.Actor + ":" + e.Kind
		if positions[key] == 0 {
			positions[key] = index + 1
		} else {
			switch e.Kind {
			case "prepared", "registered", "armed", "go_authorized", "go_received", "fault_injected":
				return fmt.Errorf("duplicate protocol transition")
			}
		}
	}
	if c.Registered && (positions["launcher:prepared"] == 0 || positions["app:registered"] <= positions["launcher:prepared"]) {
		return fmt.Errorf("registration before PREPARED")
	}
	if c.Armed && (positions["guardian:armed"] <= positions["app:registered"] || positions["app:armed"] <= positions["guardian:armed"]) {
		return fmt.Errorf("armed acknowledgement order invalid")
	}
	if c.GoAuthorized && (positions["app:go_authorized"] <= positions["app:armed"] || positions["launcher:go_received"] <= positions["app:go_authorized"]) {
		return fmt.Errorf("GO before armed acknowledgement")
	}
	if startup {
		switch c.Window {
		case "before-spawn":
			if c.LauncherPID != 0 || c.Registered || c.Armed {
				return fmt.Errorf("wrong before-spawn state")
			}
		case "after-spawn":
			if c.LauncherPID <= 0 || c.Registered || c.Armed {
				return fmt.Errorf("wrong after-spawn state")
			}
		case "registered":
			if !c.Registered || c.Armed {
				return fmt.Errorf("wrong registered state")
			}
		case "armed":
			if !c.Registered || !c.Armed {
				return fmt.Errorf("wrong armed state")
			}
		}
	}
	if c.Registered && !kinds["registered"] {
		return fmt.Errorf("registration event missing")
	}
	if c.Armed && !kinds["armed"] {
		return fmt.Errorf("armed event missing")
	}
	if c.GoAuthorized && (!kinds["go_authorized"] || !kinds["go_received"]) {
		return fmt.Errorf("GO events missing")
	}
	if c.FaultTarget != "" && (!kinds["fault_injected"] || c.FaultAtNS <= 0) {
		return fmt.Errorf("real fault not observed")
	}
	if c.FaultTarget != "" {
		injected := positions["harness:fault_injected"]
		ready := false
		for i, e := range c.Events {
			if e.Kind == "fault_injected" {
				target := c.AppPID
				if c.FaultTarget == "guardian" {
					target = c.GuardianPID
				}
				if e.Actor != "harness" || e.TargetPID != target || e.AtNS < c.FaultAtNS {
					return fmt.Errorf("fault target/time mismatch")
				}
			}
			if startup && e.Kind == "fault_window" && e.Detail == c.Window && i+1 < injected {
				actor := "app"
				if c.Window == "before-spawn" || c.Window == "after-spawn" {
					actor = "guardian"
				}
				ready = e.Actor == actor
			}
			if !startup && e.Kind == "payload_entered" && e.TargetPID == c.LauncherPID && i+1 < injected {
				ready = true
			}
		}
		if !ready {
			return fmt.Errorf("fault did not hit declared window")
		}
	}
	if c.FaultTarget == "app" && c.LauncherPID > 0 {
		r, ok := c.Receipts["guardian"]
		if !ok || !r.Waited || r.WaitedPID != c.LauncherPID {
			return fmt.Errorf("guardian did not Wait its launcher")
		}
	}
	if c.FaultTarget == "guardian" {
		r, ok := c.Receipts["app"]
		if !ok || !r.Waited || r.WaitedPID != c.GuardianPID {
			return fmt.Errorf("app did not Wait its guardian")
		}
		if r.WaitedPID == c.LauncherPID && c.LauncherPID > 0 {
			return fmt.Errorf("app falsely Waited an orphan")
		}
	}
	if c.FaultTarget == "" {
		g, ok := c.Receipts["guardian"]
		if !ok || !g.Waited || g.WaitedPID != c.LauncherPID {
			return fmt.Errorf("worker exit not Waited")
		}
		switch c.ID {
		case "normal":
			if g.ExitCode != 0 || g.StopReason != "" {
				return fmt.Errorf("normal did not exit normally")
			}
		case "error":
			if g.ExitCode != 1 || g.StopReason != "" {
				return fmt.Errorf("expected error missing")
			}
		case "cancel":
			if g.ExitCode != -1 || g.StopReason != "cancel" || !kinds["cancel_requested"] {
				return fmt.Errorf("real cancellation missing")
			}
		case "exit-cancel-race":
			natural := g.ExitCode == 0 && (g.StopReason == "" || g.StopReason == "cancel")
			cancelled := g.ExitCode == -1 && g.StopReason == "cancel"
			if !kinds["cancel_requested"] || (!natural && !cancelled) {
				return fmt.Errorf("cancel/exit race missing")
			}
		}
	}
	owner := "app"
	ownerPID := c.AppPID
	if c.FaultTarget == "app" {
		owner = "guardian"
		ownerPID = c.GuardianPID
	}
	r, ok := c.Receipts[owner]
	if !ok || r.Actor != owner || r.PID != ownerPID || r.Error != "" || !r.Cleanup || !r.TargetAbsent {
		return fmt.Errorf("mechanism receipt contradicts completion")
	}
	if r.SignalSkipped {
		if r.SignalSkipReason == "" {
			return fmt.Errorf("signal skip reason missing")
		}
		if owner == "guardian" {
			if r.SignalResolvedBy != "own_wait_and_absence" || !r.Waited || r.WaitedPID != c.LauncherPID {
				return fmt.Errorf("skipped signal not resolved by owned Wait")
			}
		} else if r.SignalResolvedBy != "registered_absence" || !c.Registered || !r.Registered {
			return fmt.Errorf("skipped signal not resolved by registered absence")
		}
	}
	return nil
}
