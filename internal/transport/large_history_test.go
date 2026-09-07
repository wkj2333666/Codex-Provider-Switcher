package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/coder/websocket"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/modelroute"
)

func TestSanitationUsesIndexForPathWithoutTrustingProvider(t *testing.T) {
	for _, stalePath := range []bool{false, true} {
		t.Run(fmt.Sprint("stalePath=", stalePath), func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, "sessions")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "rollout.jsonl")
			if err := os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"thr-a"}}`+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			var current *session
			var methods []bool
			current = newTestSession(t, func(ctx context.Context, _ websocket.MessageType, data []byte) error {
				m, err := parseRPCMessage(data)
				if err != nil {
					return err
				}
				indexed := string(m.params["useStateDbOnly"]) == "true"
				methods = append(methods, indexed)
				listed := path
				if stalePath && indexed {
					listed = "/obsolete/rollout.jsonl"
				}
				return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(`{"id":%s,"result":{"data":[{"id":"thr-a","path":%q,"modelProvider":"glm"}],"nextCursor":null}}`, m.idKey, listed)))
			}, nil)
			current.codexHome = home
			if _, err := current.sanitizeThreadRollout(context.Background(), "thr-a", "openai"); err != nil {
				t.Fatal(err)
			}
			if len(methods) == 0 || !methods[0] {
				t.Fatalf("path lookup scanned histories: %v", methods)
			}
			want := 1
			if stalePath {
				want = 2
			}
			if len(methods) != want || stalePath && methods[1] {
				t.Fatalf("fallback lookups = %v", methods)
			}
			if current.effectiveProvider("thr-a") != "" {
				t.Fatal("index-only metadata became runtime proof")
			}
		})
	}
}

func TestInternalResumesNeverHydrateHistory(t *testing.T) {
	for _, operation := range []string{"handoff", "resubscribe", "restore"} {
		t.Run(operation, func(t *testing.T) {
			var current *session
			writer := func(ctx context.Context, _ websocket.MessageType, data []byte) error {
				message, err := parseRPCMessage(data)
				if err != nil {
					return err
				}
				if string(message.params["excludeTurns"]) != "true" {
					t.Error("internal resume requested full history instead of metadata")
				}
				return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(`{"id":%s,"result":{"thread":{"id":"thr-a","turns":[]},"modelProvider":"openai","model":"gpt-5.6-sol"}}`, message.idKey)))
			}
			current = newTestSession(t, writer, nil)
			// Desktop templates may explicitly request history. Internal calls
			// consume only routing metadata and must override that preference.
			current.resumeTemplates["thr-a"] = map[string]json.RawMessage{"excludeTurns": json.RawMessage("false")}
			current.setDetached("thr-a", true)
			route := modelroute.Route{Provider: "openai", Model: "gpt-5.6-sol"}
			var err error
			switch operation {
			case "handoff":
				err = current.internalResume(context.Background(), "thr-a", route)
			case "resubscribe":
				_, err = current.Resubscribe(context.Background(), "thr-a", route)
			case "restore":
				_, err = current.Restore(context.Background(), "thr-a")
			}
			if err != nil {
				t.Fatal(err)
			}
			if current.effectiveRoute("thr-a") != route {
				t.Fatal("metadata-only resume did not verify route")
			}
			if string(current.resumeTemplates["thr-a"]["excludeTurns"]) != "false" {
				t.Fatal("internal resume changed Desktop history preference")
			}
		})
	}
}
