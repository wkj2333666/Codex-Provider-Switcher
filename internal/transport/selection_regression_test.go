package transport

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/coder/websocket"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/modelroute"
)

// A durable selection cannot prove that the loaded runtime switched providers.
func TestSessionExactSelectionRequiresRuntimeHandoff(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		selected string
		command  bool
		cold     bool
		mismatch bool
	}{
		{name: "switch from saved glm", selected: "glm", command: true},
		{name: "repair saved openai on switch", selected: "openai", command: true},
		{name: "repair saved openai before message", selected: "openai"},
		{name: "resolve runtime without cache", selected: "openai", cold: true},
		{name: "reject failed switch", selected: "glm", command: true, mismatch: true},
		{name: "block message on failed repair", selected: "openai", mismatch: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var current *session
			var upstream, downstream messageRecorder
			runtimeProvider := "glm"
			writer := func(ctx context.Context, typ websocket.MessageType, payload []byte) error {
				if err := upstream.write(ctx, typ, payload); err != nil {
					return err
				}
				msg, err := parseRPCMessage(payload)
				if err != nil {
					return err
				}
				switch msg.method {
				case "thread/list":
					return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(`{"id":%s,"result":{"data":[{"id":"thr-a","modelProvider":"glm","model":"glm-5.2"}],"nextCursor":null}}`, msg.idKey)))
				case "thread/resume":
					assertRawString(t, msg.params, "modelProvider", "openai")
					assertRawString(t, msg.params, "model", "gpt-5.6-sol")
					if !tt.mismatch {
						runtimeProvider = "openai"
					}
					return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(`{"id":%s,"result":{"thread":{"id":"thr-a"},"modelProvider":%q,"model":"gpt-5.6-sol"}}`, msg.idKey, runtimeProvider)))
				case "turn/start":
					if runtimeProvider != "openai" {
						t.Error("user message sent through GLM before provider handoff")
					}
					assertRawString(t, msg.params, "model", "gpt-5.6-sol")
					return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(`{"id":%s,"result":{"turn":{"id":"turn-a","status":"inProgress"}}}`, msg.idKey)))
				}
				return nil
			}
			current = newTestSession(t, writer, downstream.write)
			current.provider = ""
			current.routes = testModelCatalog(t)
			selectedModel := "gpt-5.6-sol"
			if tt.selected == "glm" {
				selectedModel = "glm-5.2"
			}
			selections := &fakeProviderSelections{values: map[string]string{"thr-a": tt.selected}, models: map[string]string{"thr-a": selectedModel}}
			current.selections = selections
			current.coordinator = &fakeHandoffCoordinator{}
			if !tt.cold {
				current.effective["thr-a"] = "glm"
				current.effectiveModel["thr-a"] = "glm-5.2"
			}
			input := "hello"
			if tt.command {
				input = "/provider switch openai"
			}
			request := []byte(fmt.Sprintf(`{"id":41,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":%q}]}}`, input))
			if err := current.handleDownstreamText(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			if tt.mismatch {
				for _, payload := range upstream.messages() {
					msg, _ := parseRPCMessage(payload)
					if msg.method == "turn/start" {
						t.Error("failed repair forwarded user message")
					}
				}
				messages := downstream.messages()
				if len(messages) != 1 || !bytes.Contains(messages[0], []byte(`"error"`)) {
					t.Fatalf("expected handoff error, got %q", messages)
				}
				if selections.values["thr-a"] != tt.selected {
					t.Fatal("failed switch overwrote saved provider")
				}
				return
			}
			if runtimeProvider != "openai" {
				t.Fatal("switch reported success without moving runtime off GLM")
			}
			if got := current.effectiveRoute("thr-a"); got != (modelroute.Route{Provider: "openai", Model: "gpt-5.6-sol"}) {
				t.Fatalf("runtime route = %#v", got)
			}
			if tt.command {
				if selections.values["thr-a"] != "openai" || selections.models["thr-a"] != "gpt-5.6-sol" {
					t.Fatal("verified route not persisted")
				}
				if got := syntheticAgentFeedback(t, downstream.messages()); got != "Provider switched to openai using model gpt-5.6-sol." {
					t.Fatalf("feedback = %q", got)
				}
			}
		})
	}
}

// turn/start only changes the model; its acknowledgement proves no provider.
func TestSessionTurnAcknowledgementDoesNotInventProvider(t *testing.T) {
	t.Parallel()
	current := newTestSession(t, nil, nil)
	current.effective["thr-a"] = "glm"
	current.desktop["1"] = &desktopRequest{method: "turn/start", threadID: "thr-a", targetRoute: modelroute.Route{Provider: "openai", Model: "gpt-5.6-sol"}}
	if err := current.handleUpstreamText(context.Background(), []byte(`{"id":1,"result":{"turn":{"id":"turn-a"}}}`)); err != nil {
		t.Fatal(err)
	}
	if got := current.effectiveProvider("thr-a"); got != "glm" {
		t.Fatalf("turn acknowledgement invented provider %q", got)
	}
}
