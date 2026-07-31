package rewrite

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestLineRewritesRoutingMethods(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		method    string
		wantKey   string
		wantValue any
		wantOther any
	}{
		{name: "start missing params", input: `{"jsonrpc":"2.0","id":1,"method":"thread/start"}`, method: "thread/start", wantKey: "modelProvider", wantValue: "provider-a"},
		{name: "resume null params", input: `{"jsonrpc":"2.0","id":"r","method":"thread/resume","params":null}`, method: "thread/resume", wantKey: "modelProvider", wantValue: "provider-a"},
		{name: "fork overwrites provider", input: `{"jsonrpc":"2.0","id":3,"method":"thread/fork","params":{"modelProvider":"other","keep":42}}`, method: "thread/fork", wantKey: "modelProvider", wantValue: "provider-a", wantOther: float64(42)},
		{name: "list removes filter", input: `{"jsonrpc":"2.0","id":4,"method":"thread/list","params":{"modelProviders":["other"],"limit":10}}`, method: "thread/list", wantKey: "modelProviders", wantValue: []any{}, wantOther: float64(10)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Line([]byte(tt.input), "provider-a")
			if err != nil {
				t.Fatalf("Line() error = %v", err)
			}

			var message map[string]any
			if err := json.Unmarshal(got, &message); err != nil {
				t.Fatalf("rewritten message is invalid JSON: %v", err)
			}
			if message["method"] != tt.method {
				t.Fatalf("method = %v, want %q", message["method"], tt.method)
			}
			params, ok := message["params"].(map[string]any)
			if !ok {
				t.Fatalf("params = %#v, want object", message["params"])
			}
			if !valuesEqual(params[tt.wantKey], tt.wantValue) {
				t.Errorf("params[%q] = %#v, want %#v", tt.wantKey, params[tt.wantKey], tt.wantValue)
			}
			if tt.wantOther != nil && params["keep"] != tt.wantOther && params["limit"] != tt.wantOther {
				t.Errorf("unrelated params were not preserved: %#v", params)
			}
		})
	}
}

func TestLinePreservesUnrelatedFieldsSemantically(t *testing.T) {
	t.Parallel()

	input := []byte(`{"jsonrpc":"2.0","id":{"nested":1},"method":"thread/start","params":{"cwd":"/tmp/work"},"extension":{"enabled":true}}`)
	got, err := Line(input, "provider-a")
	if err != nil {
		t.Fatalf("Line() error = %v", err)
	}

	var message map[string]any
	if err := json.Unmarshal(got, &message); err != nil {
		t.Fatal(err)
	}
	if message["extension"].(map[string]any)["enabled"] != true {
		t.Errorf("extension field was not preserved: %s", got)
	}
	if message["id"].(map[string]any)["nested"] != float64(1) {
		t.Errorf("id was not preserved: %s", got)
	}
}

func TestLinePassesUnknownMethodThroughByteForByte(t *testing.T) {
	t.Parallel()

	input := []byte(` { "jsonrpc": "2.0", "method": "custom/do", "params": [1, 2] } `)
	got, err := Line(input, "provider-a")
	if err != nil {
		t.Fatalf("Line() error = %v", err)
	}
	if !bytes.Equal(got, input) {
		t.Errorf("Line() = %q, want byte-for-byte %q", got, input)
	}
}

func TestLineRejectsInvalidMessages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{name: "malformed JSON", input: `{"method":`},
		{name: "top-level array", input: `[{"method":"thread/start"}]`},
		{name: "string params", input: `{"method":"thread/start","params":"secret-value"}`},
		{name: "array params", input: `{"method":"thread/list","params":[]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Line([]byte(tt.input), "provider-a")
			if err == nil {
				t.Fatal("Line() error = nil, want failure")
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("error leaked input body: %v", err)
			}
		})
	}
}

func valuesEqual(got, want any) bool {
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	return bytes.Equal(gotJSON, wantJSON)
}
