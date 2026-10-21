package ui

import "testing"

func TestToolkitVersion(t *testing.T) {
	const want = "0.6.0" // mygo v0.6.0，commit 081639f2a55d62a09aca742684827937dac6cb39
	if got := ToolkitVersion(); got != want {
		t.Fatalf("ToolkitVersion() = %q, want %q", got, want)
	}
}
