package transport

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wkj2333666/Codex-Provider-Switcher/internal/modelroute"
)

func TestParseRPCMessageExtractsRoutingMetadata(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		kind     rpcKind
		method   string
		id       string
		threadID string
	}{
		{
			name:  "numeric turn request",
			input: `{"id":7,"method":"turn/start","params":{"threadId":"thr-a","input":[]}}`,
			kind:  rpcRequest, method: "turn/start", id: "7", threadID: "thr-a",
		},
		{
			name:  "string resume request",
			input: `{"id":"resume-1","method":"thread/resume","params":{"threadId":"thr-b"}}`,
			kind:  rpcRequest, method: "thread/resume", id: `"resume-1"`, threadID: "thr-b",
		},
		{
			name:  "turn completed notification",
			input: `{"method":"turn/completed","params":{"threadId":"thr-a","turn":{"id":"turn-1"}}}`,
			kind:  rpcNotification, method: "turn/completed", threadID: "thr-a",
		},
		{
			name:  "response",
			input: `{"id":7,"result":{"thread":{"id":"thr-a"},"modelProvider":"sub2api"}}`,
			kind:  rpcResponse, id: "7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseRPCMessage([]byte(tt.input))
			if err != nil {
				t.Fatal(err)
			}
			if got.kind != tt.kind || got.method != tt.method || got.idKey != tt.id || got.threadID != tt.threadID {
				t.Fatalf("parse = %#v", got)
			}
		})
	}
}

func TestParseRPCMessageRejectsInvalidTopLevelWithoutLeaking(t *testing.T) {
	t.Parallel()
	for _, input := range []string{
		`not-json-secret`,
		`["secret-array"]`,
		`null`,
	} {
		_, err := parseRPCMessage([]byte(input))
		if err == nil {
			t.Fatalf("parseRPCMessage(%q) error = nil", input)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatalf("error leaked payload: %v", err)
		}
	}
}

func TestRequireThreadIDRejectsMalformedTurnStart(t *testing.T) {
	t.Parallel()
	for _, input := range []string{
		`{"id":1,"method":"turn/start"}`,
		`{"id":1,"method":"turn/start","params":[]}`,
		`{"id":1,"method":"turn/start","params":{"threadId":7}}`,
		`{"id":1,"method":"turn/start","params":{"threadId":""}}`,
	} {
		message, err := parseRPCMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		_, err = requireThreadID(message)
		if err == nil || strings.Contains(err.Error(), "threadId\":") {
			t.Fatalf("requireThreadID() error = %v", err)
		}
	}
}

func TestResponseThreadRoute(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		input        string
		wantThreadID string
		wantRoute    modelroute.Route
		wantOK       bool
	}{
		{
			name: "mapped route", input: `{"id":1,"result":{"thread":{"id":"thr-a"},"model":"glm-5.2","modelProvider":"glm"}}`,
			wantThreadID: "thr-a", wantRoute: modelroute.Route{Provider: "glm", Model: "glm-5.2"}, wantOK: true,
		},
		{
			name: "provider-only legacy route", input: `{"id":2,"result":{"thread":{"id":"thr-b"},"modelProvider":"sub2api"}}`,
			wantThreadID: "thr-b", wantRoute: modelroute.Route{Provider: "sub2api"}, wantOK: true,
		},
		{name: "malformed thread", input: `{"id":3,"result":{"thread":{"id":7},"modelProvider":"glm","model":"glm-5.2"}}`},
		{name: "missing provider", input: `{"id":4,"result":{"thread":{"id":"thr-a"},"model":"glm-5.2"}}`},
		{name: "malformed model", input: `{"id":5,"result":{"thread":{"id":"thr-a"},"modelProvider":"glm","model":52}}`},
		{name: "rpc error", input: `{"id":6,"error":{"code":500,"message":"secret"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			message, err := parseRPCMessage([]byte(tt.input))
			if err != nil {
				t.Fatal(err)
			}
			threadID, route, ok := responseThreadRoute(message)
			if ok != tt.wantOK || threadID != tt.wantThreadID || route != tt.wantRoute {
				t.Fatalf("responseThreadRoute() = %q, %#v, %v; want %q, %#v, %v", threadID, route, ok, tt.wantThreadID, tt.wantRoute, tt.wantOK)
			}
		})
	}
}

func TestEncodeRPCRequestAndError(t *testing.T) {
	t.Parallel()
	request, err := encodeRPCRequest("internal-1", "thread/unsubscribe", map[string]json.RawMessage{
		"threadId": json.RawMessage(`"thr-a"`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var requestObject map[string]any
	if err := json.Unmarshal(request, &requestObject); err != nil {
		t.Fatal(err)
	}
	if requestObject["id"] != "internal-1" || requestObject["method"] != "thread/unsubscribe" {
		t.Fatalf("request = %#v", requestObject)
	}

	for _, id := range []json.RawMessage{json.RawMessage(`7`), json.RawMessage(`"desktop-7"`)} {
		encoded, err := encodeRPCError(id, -32090, "provider handoff unavailable")
		if err != nil {
			t.Fatal(err)
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &envelope); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(envelope["id"], id) {
			t.Fatalf("encoded id = %s, want %s", envelope["id"], id)
		}
		if bytes.Contains(encoded, []byte("thr-a")) {
			t.Fatalf("error leaked thread id: %s", encoded)
		}
	}
}
