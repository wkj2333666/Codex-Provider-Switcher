package transport

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseProviderCommandRecognizesStrictControlInputs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		provider string
	}{
		{
			name: "skill invocation",
			input: `{"id":1,"method":"turn/start","params":{"threadId":"thr-a","input":[` +
				`{"type":"text","text":"$provider sub2api","text_elements":[]},` +
				`{"type":"skill","name":"provider","path":"/home/user/.agents/skills/provider/SKILL.md"}]}}`,
			provider: "sub2api",
		},
		{
			name: "skill item before text",
			input: `{"id":2,"method":"turn/start","params":{"threadId":"thr-a","input":[` +
				`{"type":"skill","name":"provider","path":"/skill/SKILL.md"},` +
				`{"type":"text","text":"  $provider openai  "}]}}`,
			provider: "openai",
		},
		{
			name:     "plain slash form",
			input:    `{"id":3,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"/provider provider_1.test"}]}}`,
			provider: "provider_1.test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			message, err := parseRPCMessage([]byte(tt.input))
			if err != nil {
				t.Fatal(err)
			}
			got, recognized, err := parseProviderCommand(message)
			if err != nil || !recognized || got != tt.provider {
				t.Fatalf("parseProviderCommand() = %q, %v, %v", got, recognized, err)
			}
		})
	}
}

func TestParseProviderCommandRejectsMalformedControlsWithoutLeaking(t *testing.T) {
	t.Parallel()
	inputs := []string{
		`{"id":1,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"/provider"}]}}`,
		`{"id":1,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"/provider secret/bad"}]}}`,
		`{"id":1,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"/provider openai trailing secret"}]}}`,
		`{"id":1,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"/provider openai"},{"type":"image","url":"secret-url"}]}}`,
		`{"id":1,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"/provider openai"},"secret-invalid-item"]}}`,
		`{"id":1,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"$provider"},{"type":"skill","name":"provider","path":"/secret/SKILL.md"}]}}`,
		`{"id":1,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"$provider openai"},{"type":"text","text":"secret"},{"type":"skill","name":"provider","path":"/skill/SKILL.md"}]}}`,
	}
	for _, input := range inputs {
		message, err := parseRPCMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		_, recognized, err := parseProviderCommand(message)
		if !recognized || err == nil {
			t.Fatalf("parseProviderCommand(%q) = recognized %v, error %v", input, recognized, err)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatalf("error leaked input: %v", err)
		}
	}
}

func TestParseProviderCommandForwardsOrdinaryMessages(t *testing.T) {
	t.Parallel()
	inputs := []string{
		`{"id":1,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"Explain /provider openai to me"}]}}`,
		`{"id":1,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"$provider openai"}]}}`,
		`{"id":1,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"$other openai"},{"type":"skill","name":"other","path":"/skill/SKILL.md"}]}}`,
		`{"id":1,"method":"turn/start","params":{"threadId":"thr-a","input":[]}}`,
	}
	for _, input := range inputs {
		message, err := parseRPCMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		provider, recognized, err := parseProviderCommand(message)
		if err != nil || recognized || provider != "" {
			t.Fatalf("parseProviderCommand(%q) = %q, %v, %v", input, provider, recognized, err)
		}
	}
}

func TestEncodeProviderSwitchTurnMatchesAppServerV2Lifecycle(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 123_000_000)
	messages, err := encodeProviderSwitchTurn(json.RawMessage(`9`), "thr-a", "sub2api", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 8 {
		t.Fatalf("message count = %d, want 8", len(messages))
	}
	wantMethods := []string{"", "turn/started", "item/started", "item/completed", "item/started", "item/agentMessage/delta", "item/completed", "turn/completed"}

	var turnID, userItemID, agentItemID string
	for index, payload := range messages {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(payload, &envelope); err != nil {
			t.Fatalf("message %d invalid JSON: %v", index, err)
		}
		var method string
		_ = json.Unmarshal(envelope["method"], &method)
		if method != wantMethods[index] {
			t.Fatalf("message %d method = %q, want %q", index, method, wantMethods[index])
		}

		if index == 0 {
			if string(envelope["id"]) != "9" {
				t.Fatalf("response id = %s", envelope["id"])
			}
			result := decodeObject(envelope["result"])
			turn := decodeObject(result["turn"])
			assertTurnFields(t, turn, "inProgress", "notLoaded", nil, nil)
			_ = json.Unmarshal(turn["id"], &turnID)
			if turnID == "" {
				t.Fatal("response turn id is empty")
			}
			continue
		}

		params := decodeObject(envelope["params"])
		var gotThreadID, gotTurnID string
		_ = json.Unmarshal(params["threadId"], &gotThreadID)
		if method != "turn/started" && method != "turn/completed" {
			_ = json.Unmarshal(params["turnId"], &gotTurnID)
		}
		if gotThreadID != "thr-a" || (gotTurnID != "" && gotTurnID != turnID) {
			t.Fatalf("message %d routing = %q, %q", index, gotThreadID, gotTurnID)
		}

		switch index {
		case 1:
			turn := decodeObject(params["turn"])
			assertTurnFields(t, turn, "inProgress", "notLoaded", float64(now.Unix()), nil)
		case 2, 3:
			item := decodeObject(params["item"])
			var itemType, itemID string
			_ = json.Unmarshal(item["type"], &itemType)
			_ = json.Unmarshal(item["id"], &itemID)
			if index == 2 {
				userItemID = itemID
			}
			if itemType != "userMessage" || itemID == "" || itemID != userItemID || string(item["clientId"]) != "null" {
				t.Fatalf("user item %d = %#v", index, item)
			}
			var content []map[string]json.RawMessage
			if json.Unmarshal(item["content"], &content) != nil || len(content) != 1 || string(content[0]["text_elements"]) != "[]" {
				t.Fatalf("user content = %s", item["content"])
			}
		case 4:
			item := decodeObject(params["item"])
			_ = json.Unmarshal(item["id"], &agentItemID)
			if agentItemID == "" || string(item["text"]) != `""` || string(item["phase"]) != `"final_answer"` || string(item["memoryCitation"]) != "null" {
				t.Fatalf("started agent item = %#v", item)
			}
		case 5:
			var itemID, delta string
			_ = json.Unmarshal(params["itemId"], &itemID)
			_ = json.Unmarshal(params["delta"], &delta)
			if itemID != agentItemID || delta != "Provider switched to sub2api." {
				t.Fatalf("agent delta = %q, %q", itemID, delta)
			}
		case 6:
			item := decodeObject(params["item"])
			var itemID, text string
			_ = json.Unmarshal(item["id"], &itemID)
			_ = json.Unmarshal(item["text"], &text)
			if itemID != agentItemID || text != "Provider switched to sub2api." {
				t.Fatalf("completed agent item = %q, %q", itemID, text)
			}
		case 7:
			turn := decodeObject(params["turn"])
			assertTurnFields(t, turn, "completed", "summary", float64(now.Unix()), float64(now.Unix()))
			var items []map[string]json.RawMessage
			if json.Unmarshal(turn["items"], &items) != nil || len(items) != 1 {
				t.Fatalf("completed turn items = %s", turn["items"])
			}
			var itemID string
			_ = json.Unmarshal(items[0]["id"], &itemID)
			if itemID != agentItemID {
				t.Fatalf("summary item id = %q, want %q", itemID, agentItemID)
			}
		}
	}
}

func assertTurnFields(t *testing.T, turn map[string]json.RawMessage, status, itemsView string, startedAt, completedAt any) {
	t.Helper()
	var gotStatus, gotItemsView string
	_ = json.Unmarshal(turn["status"], &gotStatus)
	_ = json.Unmarshal(turn["itemsView"], &gotItemsView)
	if gotStatus != status || gotItemsView != itemsView || string(turn["error"]) != "null" || string(turn["durationMs"]) != "null" {
		t.Fatalf("turn fields = %#v", turn)
	}
	assertJSONValue(t, turn["startedAt"], startedAt)
	assertJSONValue(t, turn["completedAt"], completedAt)
}

func assertJSONValue(t *testing.T, raw json.RawMessage, want any) {
	t.Helper()
	var got any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("JSON value = %#v, want %#v", got, want)
	}
}
