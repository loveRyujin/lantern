package llm

import (
	"errors"
	"strings"
	"testing"
)

func TestReadSSEEvents(t *testing.T) {
	in := strings.Join([]string{
		": keep-alive",
		"",
		"event: message",
		"data: {",
		`data: "a": 1`,
		"data: }",
		"",
		"data: [DONE]",
		"",
	}, "\r\n")
	var got []string
	err := readSSE(strings.NewReader(in), func(payload string) error {
		got = append(got, payload)
		return nil
	})
	if !errors.Is(err, errStreamEnd) {
		t.Fatal(err)
	}
	want := "{\n\"a\": 1\n}"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("%q", got)
	}
}

func TestReadSSEDoneWithoutTrailingNewline(t *testing.T) {
	err := readSSE(strings.NewReader("data: [DONE]"), func(string) error {
		t.Fatal("payload")
		return nil
	})
	if !errors.Is(err, errStreamEnd) {
		t.Fatal(err)
	}
}

func TestReadSSELongLine(t *testing.T) {
	payload := `{"content":"` + strings.Repeat("a", 70*1024) + `"}`
	in := "data: " + payload + "\n\ndata: [DONE]\n"
	var got string
	err := readSSE(strings.NewReader(in), func(p string) error {
		got = p
		return nil
	})
	if !errors.Is(err, errStreamEnd) {
		t.Fatal(err)
	}
	if got != payload {
		t.Fatalf("len %d want %d", len(got), len(payload))
	}
}

func TestReadSSELineTooLong(t *testing.T) {
	in := "data: " + strings.Repeat("a", maxSSELine) + "\n"
	err := readSSE(strings.NewReader(in), func(string) error { return nil })
	if !errors.Is(err, errLineTooLong) {
		t.Fatalf("%v", err)
	}
}

func TestReadSSEPropagatesCallbackError(t *testing.T) {
	sentinel := errors.New("stop")
	err := readSSE(strings.NewReader("data: hi\n\n"), func(string) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
}
