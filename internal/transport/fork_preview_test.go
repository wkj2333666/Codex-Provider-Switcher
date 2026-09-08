package transport

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

func TestForkResponseRepairsPreviewBeforeAcknowledgement(t *testing.T) {
	var messages [][]byte
	repaired := false
	current := newTestSession(t, nil, func(_ context.Context, _ websocket.MessageType, data []byte) error {
		if !repaired {
			t.Fatal("fork acknowledgement preceded metadata repair")
		}
		messages = append(messages, append([]byte(nil), data...))
		return nil
	})
	current.codexHome = t.TempDir()
	current.repairForkPreview = func(_ context.Context, home, id string) (string, error) {
		if home != current.codexHome || id != "child" {
			t.Fatalf("unexpected target %q %q", home, id)
		}
		repaired = true
		return "inherited user request", nil
	}
	ctx := context.Background()
	if err := current.handleDownstreamText(ctx, []byte(`{"id":17,"method":"thread/fork","params":{"threadId":"parent"}}`)); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"id":17,"result":{"thread":{"id":"child","historyMode":"paginated","preview":"","turns":[],"extra":{"keep":true}},"modelProvider":"openai","model":"gpt-test"}}`)
	if err := current.handleUpstreamText(ctx, payload); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || !strings.Contains(string(messages[0]), `"preview":"inherited user request"`) || !strings.Contains(string(messages[0]), `"keep":true`) || !strings.Contains(string(messages[0]), `"id":17`) {
		t.Fatalf("incorrect fork reply: %s", messages)
	}
}

func TestPreviewRepairOnlyRunsForEmptyPersistedPaginatedForkReplies(t *testing.T) {
	for _, tc := range []struct{ name, method, payload string }{
		{"resume", "thread/resume", `{"id":1,"result":{"thread":{"id":"child","historyMode":"paginated","preview":""}}}`},
		{"legacy", "thread/fork", `{"id":1,"result":{"thread":{"id":"child","historyMode":"legacy","preview":""}}}`},
		{"ephemeral", "thread/fork", `{"id":1,"result":{"thread":{"id":"child","historyMode":"paginated","ephemeral":true,"preview":""}}}`},
		{"existing", "thread/fork", `{"id":1,"result":{"thread":{"id":"child","historyMode":"paginated","preview":"native fixed"}}}`},
		{"failure", "thread/fork", `{"id":1,"error":{"code":-1,"message":"fork failed"}}`},
		{"untracked", "", `{"id":1,"result":{"thread":{"id":"child","historyMode":"paginated","preview":""}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []byte
			current := newTestSession(t, nil, func(_ context.Context, _ websocket.MessageType, p []byte) error { got = p; return nil })
			current.codexHome = t.TempDir()
			current.repairForkPreview = func(context.Context, string, string) (string, error) { t.Fatal("unexpected repair"); return "", nil }
			if tc.method != "" {
				current.desktop["1"] = &desktopRequest{method: tc.method}
			}
			if err := current.handleUpstreamText(context.Background(), []byte(tc.payload)); err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.payload {
				t.Fatalf("unrelated response changed: %s", got)
			}
		})
	}
}

func TestForkPreviewFailureKeepsSuccessfulForkAndWarns(t *testing.T) {
	var got []map[string]json.RawMessage
	current := newTestSession(t, nil, func(_ context.Context, _ websocket.MessageType, p []byte) error {
		if strings.Contains(string(p), "secret database detail") {
			t.Fatal("internal error leaked")
		}
		var v map[string]json.RawMessage
		if err := json.Unmarshal(p, &v); err != nil {
			return err
		}
		got = append(got, v)
		return nil
	})
	current.codexHome = t.TempDir()
	current.repairForkPreview = func(context.Context, string, string) (string, error) { return "", errors.New("secret database detail") }
	current.desktop["1"] = &desktopRequest{method: "thread/fork"}
	if err := current.handleUpstreamText(context.Background(), []byte(`{"id":1,"result":{"thread":{"id":"child","historyMode":"paginated","preview":""}}}`)); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0]["result"] == nil || string(got[1]["method"]) != `"warning"` {
		t.Fatalf("lost fork success or warning: %v", got)
	}
}
