// lantern-sandbox-probe is a fixed, disposable experiment, not Sandbox.Run.
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

type check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}
type report struct {
	Environment  map[string]any `json:"environment,omitempty"`
	Controls     []check        `json:"controls,omitempty"`
	Backend      string         `json:"backend"`
	Status       string         `json:"status"`
	Kernel       string         `json:"kernel,omitempty"`
	ABI          int            `json:"landlock_abi,omitempty"`
	Error        string         `json:"error,omitempty"`
	Checks       []check        `json:"checks,omitempty"`
	RuntimeRoots []string       `json:"runtime_roots,omitempty"`
}

func emit(r report) { _ = json.NewEncoder(os.Stdout).Encode(r) }
func main() {
	r := run(os.Args[1:])
	emit(r)
	switch r.Status {
	case "passed":
	case "unsupported":
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "sandbox probe did not pass")
		os.Exit(1)
	}
}
