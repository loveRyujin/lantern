//go:build linux && amd64

package eval

import (
	"context"
	"testing"
	"time"
)

func TestSandboxRealNetworkRequestContract(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	executor := &linuxRealExecutor{root: t.TempDir()}
	evidence := executor.networkRequest(ctx, Case{ID: "SB-LINUX-20"})
	if evidence.Detail != "" {
		t.Fatal(evidence.Detail)
	}
	if evidence.Observations["valid_schema_accepted"] != true ||
		evidence.Observations["policy_denied"] != true ||
		evidence.Observations["payload_entered"] != false ||
		evidence.Observations["approval_ticket_count"] != 0 ||
		evidence.PayloadEntered {
		t.Fatalf("network contract observations: %+v entered=%t", evidence.Observations, evidence.PayloadEntered)
	}
	if _, ok := evidence.Observations["registry_rejected"]; ok {
		t.Fatal("registry_rejected is not the network contract")
	}
	if _, ok := evidence.Observations["tool_execute_count"]; ok {
		t.Fatal("tool_execute_count is not the network contract")
	}
}
