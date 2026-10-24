package main

import _ "embed"

//go:embed payload.py
var pythonPayload []byte

var fixedModes = map[string]bool{"filesystem": true, "network": true, "process-profile": true, "benign": true, "cpu": true, "rss": true, "vmem": true, "output": true, "stderr": true, "disk": true, "wall": true, "sleep": true, "error": true, "fontcache": true, "sql": true}

type taskConfig struct {
	Mode             string   `json:"mode"`
	Python           string   `json:"python"`
	Script           string   `json:"script"`
	Policy           string   `json:"policy"`
	Root             string   `json:"root"`
	Input            string   `json:"input"`
	Outside          string   `json:"outside"`
	Database         string   `json:"database"`
	RuntimeSentinel  string   `json:"runtime_sentinel"`
	Out              string   `json:"out"`
	Tmp              string   `json:"tmp"`
	SensitiveTargets []string `json:"sensitive_targets"`
	TCPPort          int      `json:"tcp_port"`
	UDPPort          int      `json:"udp_port"`
	UnixSocket       string   `json:"unix_socket"`
	SignalPID        int      `json:"signal_pid"`
	Sandboxed        bool     `json:"sandboxed"`
	Budget           Budget   `json:"budget"`
	Receipt          string   `json:"receipt"`
	Ready            string   `json:"ready"`
	AppReady         string   `json:"app_ready"`
}
