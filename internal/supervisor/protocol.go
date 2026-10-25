package supervisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"unicode/utf8"
)

const protocolDescription = "v1: guardian owns launcher; launcher registers directly with app; PREPARED -> app registered -> guardian armed -> app GO -> exec; all control FDs CLOEXEC; one fault only; no harness rescue"

var errIdentityUnavailable = errors.New("registered identity unavailable; no signal sent")
var errBirthMismatch = errors.New("different registered birth; no signal sent")

func classifyIdentity(expected, observed Identity, readErr error) error {
	if readErr != nil {
		return fmt.Errorf("%w: %v", errIdentityUnavailable, readErr)
	}
	if observed.PID <= 0 || observed.StartSeconds == 0 || observed.StartMicroseconds >= 1000000 {
		return fmt.Errorf("%w: invalid or incomplete identity fields", errIdentityUnavailable)
	}
	if expected.PID <= 0 || observed.PID != expected.PID || observed.StartSeconds != expected.StartSeconds || observed.StartMicroseconds != expected.StartMicroseconds {
		return fmt.Errorf("%w: expected %d/%d.%06d observed %d/%d.%06d", errBirthMismatch, expected.PID, expected.StartSeconds, expected.StartMicroseconds, observed.PID, observed.StartSeconds, observed.StartMicroseconds)
	}
	return nil
}

type receipt struct {
	Target           Identity     `json:"target"`
	Controls         []FDIdentity `json:"controls,omitempty"`
	Worker           WorkerResult `json:"worker"`
	StopReason       string       `json:"stop_reason,omitempty"`
	Waited           bool         `json:"waited"`
	Absent           bool         `json:"absent"`
	Clean            bool         `json:"clean"`
	SignalSkipped    bool         `json:"signal_skipped"`
	SignalSkipReason string       `json:"signal_skip_reason,omitempty"`
	SignalResolvedBy string       `json:"signal_resolved_by,omitempty"`
	Error            string       `json:"error,omitempty"`
}
type message struct {
	Kind    string        `json:"kind"`
	Seq     int           `json:"seq"`
	Nonce   string        `json:"nonce"`
	Target  Identity      `json:"target"`
	Worker  *WorkerResult `json:"worker,omitempty"`
	Receipt *receipt      `json:"receipt,omitempty"`
}

func decodeMessage(b []byte, nonce string, seq int) (message, error) {
	var v message
	if e := strictJSON(b, &v, 4096); e != nil {
		return v, e
	}
	if nonce == "" || seq <= 0 || v.Seq != seq || v.Nonce != nonce || v.Target.Nonce != nonce {
		return v, fmt.Errorf("%w: nonce/sequence", ErrProtocol)
	}
	switch v.Kind {
	case "BOOTSTRAP", "PREPARED", "REGISTERED", "ARMED", "GO", "CANCEL", "ENTERED", "COMPLETE":
	default:
		return v, fmt.Errorf("%w: unknown kind", ErrProtocol)
	}
	id := v.Target
	if !(v.Kind == "COMPLETE" && id.PID == 0 && id.StartSeconds == 0 && id.StartMicroseconds == 0 && id.Image == "") {
		if id.PID <= 0 || id.StartSeconds == 0 || id.StartMicroseconds >= 1000000 || !filepath.IsAbs(id.Image) || filepath.Clean(id.Image) != id.Image {
			return v, fmt.Errorf("%w: invalid process identity", ErrProtocol)
		}
	}
	if v.Kind == "ENTERED" && (v.Worker == nil || v.Worker.PID != id.PID || !v.Worker.Entered) {
		return v, ErrProtocol
	}
	if v.Kind == "COMPLETE" && (v.Receipt == nil || v.Receipt.Target != id) {
		return v, ErrProtocol
	}
	return v, nil
}
func strictJSON(b []byte, v any, max int) error {
	if len(b) == 0 || len(b) > max || !utf8.Valid(b) {
		return ErrProtocol
	}
	keys := json.NewDecoder(bytes.NewReader(b))
	if e := walkJSON(keys, 0); e != nil {
		return e
	}
	if _, e := keys.Token(); e != io.EOF {
		return ErrProtocol
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	if e := d.Decode(new(any)); e != io.EOF {
		return ErrProtocol
	}
	return nil
}
func walkJSON(d *json.Decoder, depth int) error {
	if depth > 32 {
		return ErrProtocol
	}
	token, e := d.Token()
	if e != nil {
		return e
	}
	switch token {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			key, e := d.Token()
			if e != nil {
				return e
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("%w: duplicate object key", ErrProtocol)
			}
			seen[name] = true
			if e = walkJSON(d, depth+1); e != nil {
				return e
			}
		}
		token, e = d.Token()
		if e != nil || token != json.Delim('}') {
			return ErrProtocol
		}
	case json.Delim('['):
		for d.More() {
			if e = walkJSON(d, depth+1); e != nil {
				return e
			}
		}
		token, e = d.Token()
		if e != nil || token != json.Delim(']') {
			return ErrProtocol
		}
	}
	return nil
}
