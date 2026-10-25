// lantern-darwin-probe runs repository-fixed bounded experiments, never user code.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/loveRyujin/lantern/core/pyrt"
)

type options struct {
	RuntimeConfig, Out string
	RequireFeasible    bool
}

func readRuntime(path string) (pyrt.Runtime, error) {
	var rt pyrt.Runtime
	f, e := openConfig(path)
	if e != nil {
		return rt, e
	}
	defer f.Close()
	s, e := f.Stat()
	if e != nil || !s.Mode().IsRegular() {
		return rt, fmt.Errorf("runtime config must be a regular file")
	}
	b, e := io.ReadAll(io.LimitReader(f, 65537))
	if e != nil {
		return rt, e
	}
	if len(b) > 65536 {
		return rt, fmt.Errorf("runtime config exceeds 64KiB")
	}
	var p *pyrt.Runtime
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(&p); e != nil {
		return rt, e
	}
	if e = d.Decode(new(interface{})); e != io.EOF {
		return rt, fmt.Errorf("runtime config must contain exactly one JSON value")
	}
	if p == nil {
		return rt, fmt.Errorf("runtime config null")
	}
	return *p, nil
}

func parseOptions(args []string) (options, error) {
	var o options
	f := flag.NewFlagSet("lantern-darwin-probe", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.StringVar(&o.RuntimeConfig, "runtime-config", "", "already prepared runtime JSON (no installation)")
	f.StringVar(&o.Out, "out", "", "report/artifact directory")
	f.BoolVar(&o.RequireFeasible, "require-feasible", false, "fail if production feasibility remains unresolved")
	if e := f.Parse(args); e != nil {
		return o, e
	}
	if f.NArg() != 0 || o.RuntimeConfig == "" || o.Out == "" {
		return o, fmt.Errorf("require --runtime-config FILE --out DIR; arbitrary commands/code are forbidden")
	}
	return o, nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "supervision" {
		os.Exit(supervisionCLI(os.Args[2:]))
	}
	if handled, code := internalMain(os.Args[1:]); handled {
		os.Exit(code)
	}
	o, err := parseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	r := runProbe(ctx, o)
	finalize(&r)
	if err := writeReports(o.Out, r); err != nil {
		r.Status = "failed"
		r.Error = "write report: " + err.Error()
	}
	_ = json.NewEncoder(os.Stdout).Encode(r)
	os.Exit(exitCode(r, o.RequireFeasible))
}
