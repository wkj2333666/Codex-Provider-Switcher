package rewrite

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/wkj2333666/Codex-Provider-Switcher/internal/modelroute"
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
			got, err := Line([]byte(tt.input), modelroute.Route{Provider: "provider-a"})
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

func TestLineAddsUnknownSourceKindToList(t *testing.T) {
	t.Parallel()

	input := []byte(`{"jsonrpc":"2.0","id":4,"method":"thread/list","params":{"limit":10}}`)
	got, err := Line(input, modelroute.Route{Provider: "provider-a"})
	if err != nil {
		t.Fatalf("Line() error = %v", err)
	}

	var message struct {
		Params struct {
			SourceKinds []string `json:"sourceKinds"`
		} `json:"params"`
	}
	if err := json.Unmarshal(got, &message); err != nil {
		t.Fatalf("rewritten message is invalid JSON: %v", err)
	}
	want := []string{"cli", "vscode", "unknown"}
	if !slices.Equal(message.Params.SourceKinds, want) {
		t.Fatalf("sourceKinds = %#v, want %#v", message.Params.SourceKinds, want)
	}
}

func TestLinePreservesUnrelatedFieldsSemantically(t *testing.T) {
	t.Parallel()

	input := []byte(`{"jsonrpc":"2.0","id":{"nested":1},"method":"thread/start","params":{"cwd":"/tmp/work"},"extension":{"enabled":true}}`)
	got, err := Line(input, modelroute.Route{Provider: "provider-a"})
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
	got, err := Line(input, modelroute.Route{Provider: "provider-a"})
	if err != nil {
		t.Fatalf("Line() error = %v", err)
	}
	if !bytes.Equal(got, input) {
		t.Errorf("Line() = %q, want byte-for-byte %q", got, input)
	}
}

func TestLineDefersProviderMethodsWhenNoOverrideIsSelected(t *testing.T) {
	t.Parallel()

	input := []byte(` { "jsonrpc": "2.0", "id": 1, "method": "thread/resume", "params": {"threadId":"thr-a"} } `)
	got, err := Line(input, modelroute.Route{})
	if err != nil {
		t.Fatalf("Line() error = %v", err)
	}
	if !bytes.Equal(got, input) {
		t.Fatalf("Line() = %q, want app-server default request %q", got, input)
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
			_, err := Line([]byte(tt.input), modelroute.Route{Provider: "provider-a"})
			if err == nil {
				t.Fatal("Line() error = nil, want failure")
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("error leaked input body: %v", err)
			}
		})
	}
}

func TestLineAppliesMappedModelToThreadAndTurnRequests(t *testing.T) {
	t.Parallel()
	route := modelroute.Route{Provider: "glm", Model: "glm-5.2"}
	tests := []struct {
		name         string
		input        string
		wantProvider bool
	}{
		{name: "thread start", input: `{"id":1,"method":"thread/start","params":{"modelProvider":"openai","model":"gpt-5.6-sol","keep":1}}`, wantProvider: true},
		{name: "thread resume", input: `{"id":2,"method":"thread/resume","params":{"threadId":"thr-a","model":"gpt-5.6-sol"}}`, wantProvider: true},
		{name: "thread fork", input: `{"id":3,"method":"thread/fork","params":{"threadId":"thr-a","modelProvider":"openai"}}`, wantProvider: true},
		{name: "turn start", input: `{"id":4,"method":"turn/start","params":{"threadId":"thr-a","model":"gpt-5.6-sol","input":[{"type":"text","text":"unchanged"}]}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Line([]byte(tt.input), route)
			if err != nil {
				t.Fatalf("Line() error = %v", err)
			}
			var message struct {
				Params map[string]any `json:"params"`
			}
			if err := json.Unmarshal(got, &message); err != nil {
				t.Fatal(err)
			}
			if message.Params["model"] != "glm-5.2" {
				t.Fatalf("params.model = %#v, want glm-5.2", message.Params["model"])
			}
			_, hasProvider := message.Params["modelProvider"]
			if tt.wantProvider {
				if message.Params["modelProvider"] != "glm" {
					t.Fatalf("params.modelProvider = %#v, want glm", message.Params["modelProvider"])
				}
			} else if hasProvider {
				t.Fatalf("turn/start gained modelProvider: %#v", message.Params)
			}
			if tt.name == "turn start" {
				input := message.Params["input"].([]any)[0].(map[string]any)
				if input["text"] != "unchanged" {
					t.Fatalf("turn input changed: %#v", message.Params["input"])
				}
			}
		})
	}
}

func TestLineAppliesMappedModelToCollaborationSettings(t *testing.T) {
	t.Parallel()
	route := modelroute.Route{Provider: "kimi", Model: "k3"}
	tests := []struct {
		name         string
		method       string
		wantProvider bool
	}{
		{name: "thread start", method: "thread/start", wantProvider: true},
		{name: "thread resume", method: "thread/resume", wantProvider: true},
		{name: "thread fork", method: "thread/fork", wantProvider: true},
		{name: "turn start", method: "turn/start"},
		{name: "thread settings update", method: "thread/settings/update"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input := []byte(`{"id":1,"method":"` + test.method + `","params":{"threadId":"thr-a","model":"gpt-5.6-sol","collaborationMode":{"mode":"default","settings":{"model":"gpt-5.6-sol","effort":"high","extension":{"keep":true}}}}}`)
			got, err := Line(input, route)
			if err != nil {
				t.Fatal(err)
			}
			var message struct {
				Params struct {
					Model         string `json:"model"`
					ModelProvider string `json:"modelProvider"`
					Collaboration struct {
						Settings struct {
							Model     string `json:"model"`
							Effort    string `json:"effort"`
							Extension struct {
								Keep bool `json:"keep"`
							} `json:"extension"`
						} `json:"settings"`
					} `json:"collaborationMode"`
				} `json:"params"`
			}
			if json.Unmarshal(got, &message) != nil {
				t.Fatalf("invalid rewritten message: %s", got)
			}
			if message.Params.Model != "k3" || message.Params.Collaboration.Settings.Model != "k3" {
				t.Fatalf("models = %q, %q, want k3", message.Params.Model, message.Params.Collaboration.Settings.Model)
			}
			if message.Params.Collaboration.Settings.Effort != "high" ||
				!message.Params.Collaboration.Settings.Extension.Keep {
				t.Fatalf("unrelated collaboration settings changed: %s", got)
			}
			if test.wantProvider && message.Params.ModelProvider != "kimi" {
				t.Fatalf("modelProvider = %q, want kimi", message.Params.ModelProvider)
			}
			if !test.wantProvider && message.Params.ModelProvider != "" {
				t.Fatalf("%s gained modelProvider: %s", test.method, got)
			}
		})
	}
}

func TestLineValidatesCollaborationSettingsOnlyWhenRoutingModel(t *testing.T) {
	t.Parallel()
	route := modelroute.Route{Provider: "kimi", Model: "k3"}
	tests := []struct {
		name      string
		input     string
		wantError bool
	}{
		{
			name:  "null collaboration mode",
			input: `{"id":1,"method":"turn/start","params":{"threadId":"thr-a","collaborationMode":null}}`,
		},
		{
			name:      "non-object collaboration mode",
			input:     `{"id":2,"method":"turn/start","params":{"threadId":"thr-a","collaborationMode":"unsafe"}}`,
			wantError: true,
		},
		{
			name:      "missing collaboration settings",
			input:     `{"id":3,"method":"turn/start","params":{"threadId":"thr-a","collaborationMode":{"mode":"default"}}}`,
			wantError: true,
		},
		{
			name:      "null collaboration settings",
			input:     `{"id":4,"method":"turn/start","params":{"threadId":"thr-a","collaborationMode":{"mode":"default","settings":null}}}`,
			wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := Line([]byte(test.input), route)
			if test.wantError {
				if err == nil {
					t.Fatalf("Line() = %s, want routing error", got)
				}
				if strings.Contains(err.Error(), "unsafe") {
					t.Fatalf("error leaked collaboration value: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var message struct {
				Params struct {
					Model         string `json:"model"`
					Collaboration any    `json:"collaborationMode"`
				} `json:"params"`
			}
			if json.Unmarshal(got, &message) != nil || message.Params.Model != "k3" || message.Params.Collaboration != nil {
				t.Fatalf("null collaboration rewrite = %s", got)
			}
		})
	}
}

func TestLinePreservesUnmappedTurnAndModelListByteForByte(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input []byte
		route modelroute.Route
	}{
		{name: "unmapped turn", input: []byte(` {"id":1,"method":"turn/start","params":{"model":"gpt-5.6-sol"}} `), route: modelroute.Route{Provider: "glm"}},
		{name: "model list", input: []byte(` {"id":2,"method":"model/list","params":{"includeHidden":true}} `), route: modelroute.Route{Provider: "glm", Model: "glm-5.2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Line(tt.input, tt.route)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tt.input) {
				t.Fatalf("Line() = %q, want byte-for-byte %q", got, tt.input)
			}
		})
	}
}

func valuesEqual(got, want any) bool {
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	return bytes.Equal(gotJSON, wantJSON)
}
