//go:build !darwin

package main

import (
	"encoding/json"
	"os"
)

func supervisorAPIInternal(args []string) (bool, int) { return false, 0 }
func supervisorAPICLI(args []string) int {
	_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"schema": "lantern.darwin-supervisor-api.v1", "status": "unsupported", "error": "native Darwin required"})
	return 1
}
