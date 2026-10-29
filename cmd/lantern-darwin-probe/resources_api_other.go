//go:build !darwin

package main

import (
	"encoding/json"
	"os"
)

func resourceAPICLI(args []string) int {
	_ = json.NewEncoder(os.Stdout).Encode(resourceAPIReport{Schema: resourceAPISchema, Status: "unsupported", Error: "native Darwin required"})
	return 1
}
