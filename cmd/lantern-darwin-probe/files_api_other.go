//go:build !darwin

package main

import (
	"encoding/json"
	"os"
)

func fileAPICLI(args []string) int {
	_ = args
	_ = json.NewEncoder(os.Stdout).Encode(fileAPIReport{Schema: fileAPISchema, Status: "unsupported", Error: "native Darwin required; file budget evidence was not produced"})
	return 1
}
