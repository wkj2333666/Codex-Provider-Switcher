package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/coder/websocket"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/handoff"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/modelroute"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/recovery"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/selection"
)

func TestSessionConsumesInternalResponse(t *testing.T) {
	t.Parallel()
	var downstream messageRecorder
	session := newTestSession(t, nil, downstream.write)
	waiter := make(chan rpcMessage, 1)
	session.stateMu.Lock()
	session.internal[`"cps-internal-1"`] = waiter
	session.stateMu.Unlock()

	payload := []byte(`{"id":"cps-internal-1","result":{"status":"unsubscribed"}}`)
	if err := session.handleUpstreamText(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-waiter:
		if response.idKey != `"cps-internal-1"` {
			t.Fatalf("internal response = %#v", response)
		}
	default:
		t.Fatal("internal waiter did not receive response")
	}
	if got := downstream.messages(); len(got) != 0 {
		t.Fatalf("internal response leaked downstream: %q", got)
	}
}

func TestSessionConsumesLateInternalResponseAfterCallerTimeout(t *testing.T) {
	t.Parallel()
	var upstream, downstream messageRecorder
	session := newTestSession(t, upstream.write, downstream.write)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := session.callUpstream(ctx, "thread/resume", map[string]json.RawMessage{
		"threadId": json.RawMessage(`"thr-a"`),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("callUpstream() error = %v", err)
	}
	messages := upstream.messages()
	if len(messages) != 1 {
		t.Fatalf("upstream messages = %q", messages)
	}
	request, err := parseRPCMessage(messages[0])
	if err != nil {
		t.Fatal(err)
	}
	response := []byte(fmt.Sprintf(`{"id":%s,"result":{"thread":{"id":"thr-a"},"modelProvider":"sub2api"}}`, request.idKey))
	if err := session.handleUpstreamText(context.Background(), response); err != nil {
		t.Fatal(err)
	}
	if got := downstream.messages(); len(got) != 0 {
		t.Fatalf("late internal response leaked downstream: %q", got)
	}
}

func TestSessionForwardsDesktopResponseByteForByte(t *testing.T) {
	t.Parallel()
	var downstream messageRecorder
	session := newTestSession(t, nil, downstream.write)
	payload := []byte(` { "id": 7, "result": {"ok":true} } `)
	if err := session.handleUpstreamText(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	messages := downstream.messages()
	if len(messages) != 1 || !bytes.Equal(messages[0], payload) {
		t.Fatalf("downstream messages = %q", messages)
	}
}

func TestSessionRecordsEffectiveProviderFromResumeResponse(t *testing.T) {
	t.Parallel()
	var downstream messageRecorder
	session := newTestSession(t, nil, downstream.write)
	seen := make(chan struct{})
	session.stateMu.Lock()
	session.desktop["1"] = &desktopRequest{method: "thread/resume", threadID: "thr-a", responseSeen: seen}
	session.stateMu.Unlock()

	payload := []byte(`{"id":1,"result":{"thread":{"id":"thr-a"},"modelProvider":"openai"}}`)
	if err := session.handleUpstreamText(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	select {
	case <-seen:
	default:
		t.Fatal("resume response did not signal waiter")
	}
	if got := session.effectiveProvider("thr-a"); got != "openai" {
		t.Fatalf("effective provider = %q", got)
	}
}

func TestSessionClearsFreshThreadAfterAcceptedTurn(t *testing.T) {
	t.Parallel()
	var downstream messageRecorder
	session := newTestSession(t, nil, downstream.write)
	session.stateMu.Lock()
	session.desktop["1"] = &desktopRequest{method: "thread/start"}
	session.stateMu.Unlock()

	startResponse := []byte(`{"id":1,"result":{"thread":{"id":"thr-fresh"},"modelProvider":"openai"}}`)
	if err := session.handleUpstreamText(context.Background(), startResponse); err != nil {
		t.Fatal(err)
	}
	if !session.isFresh("thr-fresh") {
		t.Fatal("thread/start response did not mark thread fresh")
	}

	session.stateMu.Lock()
	session.desktop["2"] = &desktopRequest{method: "turn/start", threadID: "thr-fresh"}
	session.stateMu.Unlock()
	if err := session.handleUpstreamText(context.Background(), []byte(`{"id":2,"result":{"turn":{"id":"turn-1"}}}`)); err != nil {
		t.Fatal(err)
	}
	if session.isFresh("thr-fresh") {
		t.Fatal("accepted turn left thread fresh")
	}
}

func TestSessionFreshRolloutPreservesExistingThreadName(t *testing.T) {
	t.Parallel()
	var current *session
	requestedName := ""
	writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		_ = json.Unmarshal(message.params["name"], &requestedName)
		return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(`{"id":%s,"result":{}}`, message.idKey)))
	}
	current = newTestSession(t, writer, nil)
	current.stateMu.Lock()
	current.fresh["thr-a"] = "Existing task name"
	current.stateMu.Unlock()

	if err := current.materializeFreshRollout(context.Background(), "thr-a", "sub2api"); err != nil {
		t.Fatal(err)
	}
	if requestedName != "Existing task name" {
		t.Fatalf("materialized name = %q, want existing name", requestedName)
	}
}

func TestSessionDefersUnsavedProviderToAppServer(t *testing.T) {
	t.Parallel()
	coordinator := &fakeHandoffCoordinator{}
	selections := &fakeProviderSelections{values: map[string]string{}}
	var current *session
	var upstream messageRecorder
	writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
		if err := upstream.write(ctx, messageType, payload); err != nil {
			return err
		}
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		if message.method == "thread/resume" {
			if _, present := message.params["modelProvider"]; present {
				return errors.New("switcher injected an unsaved provider")
			}
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"thread":{"id":"thr-a"},"modelProvider":"openai"}}`, message.idKey)))
		}
		return nil
	}
	current = newTestSession(t, writer, nil)
	current.provider = ""
	current.selections = selections
	current.coordinator = coordinator

	resume := []byte(`{"id":27,"method":"thread/resume","params":{"threadId":"thr-a"}}`)
	if err := current.handleDownstreamText(context.Background(), resume); err != nil {
		t.Fatal(err)
	}
	if got := current.effectiveProvider("thr-a"); got != "openai" {
		t.Fatalf("effective provider = %q, want app-server result openai", got)
	}

	turn := []byte(`{"id":28,"method":"turn/start","params":{"threadId":"thr-a","input":[]}}`)
	if err := current.handleDownstreamText(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	messages := upstream.messages()
	if len(messages) != 2 || !bytes.Equal(messages[1], turn) {
		t.Fatalf("upstream messages = %q", messages)
	}
	if coordinator.prepareHandoffCalls != 0 || coordinator.unsubscribeCalls != 0 || coordinator.resubscribeCalls != 0 {
		t.Fatalf("unsaved provider triggered handoff: %#v", coordinator)
	}
}

func TestSessionAppliesUnsavedEffectiveProviderCatalogModelOnFirstTurn(t *testing.T) {
	t.Parallel()
	coordinator := &fakeHandoffCoordinator{}
	selections := &fakeProviderSelections{values: map[string]string{}}
	var current *session
	var upstream messageRecorder
	writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
		if err := upstream.write(ctx, messageType, payload); err != nil {
			return err
		}
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		if message.method == "thread/resume" {
			assertRawString(t, message.params, "modelProvider", "glm")
			assertRawString(t, message.params, "model", "glm-5.2")
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"thread":{"id":"thr-a"},"modelProvider":"glm","model":"glm-5.2"}}`, message.idKey)))
		}
		return nil
	}
	current = newTestSession(t, writer, nil)
	current.provider = ""
	current.routes = testModelCatalog(t)
	current.selections = selections
	current.coordinator = coordinator
	current.stateMu.Lock()
	current.effective["thr-a"] = "glm"
	current.effectiveModel["thr-a"] = "gpt-5.6-sol"
	current.stateMu.Unlock()

	wantInput := json.RawMessage(`[{"type":"text","text":"keep me","metadata":{"parts":[1,true,null]}}]`)
	request := []byte(`{"id":29,"method":"turn/start","params":{"threadId":"thr-a","model":"gpt-5.6-sol","input":` + string(wantInput) + `}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	messages := upstream.messages()
	if len(messages) != 1 {
		t.Fatalf("upstream messages = %q, want one ordinary turn", messages)
	}
	turn, err := parseRPCMessage(messages[0])
	if err != nil {
		t.Fatal(err)
	}
	assertRawString(t, turn.params, "model", "glm-5.2")
	var gotDecoded, wantDecoded any
	if json.Unmarshal(turn.params["input"], &gotDecoded) != nil || json.Unmarshal(wantInput, &wantDecoded) != nil ||
		!reflect.DeepEqual(gotDecoded, wantDecoded) {
		t.Fatalf("turn input = %s, want semantic value %s", turn.params["input"], wantInput)
	}
	if coordinator.prepareHandoffCalls != 0 || coordinator.unsubscribeCalls != 0 ||
		coordinator.resubscribeCalls != 0 || coordinator.markDirtyCalls != 0 {
		t.Fatalf("same-provider first-turn model triggered handoff: %#v", coordinator)
	}
}

func TestNewSessionAllowsNoProviderOverride(t *testing.T) {
	t.Parallel()
	current, err := newSessionState("", nil, "/tmp/app-server.sock",
		func(context.Context, websocket.MessageType, []byte) error { return nil },
		func(context.Context, websocket.MessageType, []byte) error { return nil })
	if err != nil {
		t.Fatalf("newSessionState() error = %v", err)
	}
	current.closeState()
}

func TestSessionClearsActiveThreadOnErrorAndCompletion(t *testing.T) {
	t.Parallel()
	var downstream messageRecorder
	session := newTestSession(t, nil, downstream.write)

	session.setActive("thr-error", true)
	session.stateMu.Lock()
	session.desktop["2"] = &desktopRequest{method: "turn/start", threadID: "thr-error"}
	session.stateMu.Unlock()
	if err := session.handleUpstreamText(context.Background(), []byte(`{"id":2,"error":{"code":-1,"message":"failed"}}`)); err != nil {
		t.Fatal(err)
	}
	if session.isActive("thr-error") {
		t.Fatal("turn/start error left thread active")
	}

	session.setActive("thr-complete", true)
	if err := session.handleUpstreamText(context.Background(), []byte(`{"method":"turn/completed","params":{"threadId":"thr-complete","turn":{"id":"turn-1","status":"completed"}}}`)); err != nil {
		t.Fatal(err)
	}
	if session.isActive("thr-complete") {
		t.Fatal("turn/completed left thread active")
	}
}

func TestSessionPrepareRejectsActiveThread(t *testing.T) {
	t.Parallel()
	session := newTestSession(t, nil, nil)
	session.setActive("thr-a", true)
	if status := session.Prepare("thr-a"); status != handoff.StatusBusy {
		t.Fatalf("Prepare() = %q", status)
	}
	session.setActive("thr-a", false)
	if status := session.Prepare("thr-a"); status != handoff.StatusReady {
		t.Fatalf("Prepare() = %q", status)
	}
}

func TestSessionClearsProviderAfterDesktopUnsubscribeAndThreadClose(t *testing.T) {
	t.Parallel()
	var downstream messageRecorder
	session := newTestSession(t, nil, downstream.write)
	session.stateMu.Lock()
	session.effective["thr-unsubscribe"] = "sub2api"
	session.effective["thr-closed"] = "sub2api"
	session.fresh["thr-closed"] = ""
	session.desktop["4"] = &desktopRequest{
		method:       "thread/unsubscribe",
		threadID:     "thr-unsubscribe",
		responseSeen: make(chan struct{}),
	}
	session.stateMu.Unlock()

	if err := session.handleUpstreamText(context.Background(), []byte(`{"id":4,"result":{"status":"unsubscribed"}}`)); err != nil {
		t.Fatal(err)
	}
	if got := session.effectiveProvider("thr-unsubscribe"); got != "" {
		t.Fatalf("provider after Desktop unsubscribe = %q", got)
	}
	if err := session.handleUpstreamText(context.Background(), []byte(`{"method":"thread/closed","params":{"threadId":"thr-closed"}}`)); err != nil {
		t.Fatal(err)
	}
	if got := session.effectiveProvider("thr-closed"); got != "" {
		t.Fatalf("provider after thread/closed = %q", got)
	}
	if session.isFresh("thr-closed") {
		t.Fatal("thread/closed left thread fresh")
	}
}

func TestSessionClearsProviderWhenDesktopSubscriptionResponseIsUncertain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		request []byte
	}{
		{
			name:    "resume",
			request: []byte(`{"id":5,"method":"thread/resume","params":{"threadId":"thr-a"}}`),
		},
		{
			name:    "unsubscribe",
			request: []byte(`{"id":6,"method":"thread/unsubscribe","params":{"threadId":"thr-a"}}`),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var upstream messageRecorder
			current := newTestSession(t, upstream.write, nil)
			current.coordinator = &fakeHandoffCoordinator{}
			current.stateMu.Lock()
			current.effective["thr-a"] = "sub2api"
			current.stateMu.Unlock()

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := current.handleDownstreamText(ctx, test.request); !errors.Is(err, context.Canceled) {
				t.Fatalf("handleDownstreamText() error = %v", err)
			}
			if len(upstream.messages()) != 1 {
				t.Fatalf("upstream messages = %q", upstream.messages())
			}
			if got := current.effectiveProvider("thr-a"); got != "" {
				t.Fatalf("effective provider after uncertain response = %q", got)
			}
		})
	}
}

func TestSessionUnsubscribeConsumesResponseAndClearsProvider(t *testing.T) {
	t.Parallel()
	var current *session
	var upstream messageRecorder
	writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
		if err := upstream.write(ctx, messageType, payload); err != nil {
			return err
		}
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		response := []byte(fmt.Sprintf(`{"id":%s,"result":{"status":"unsubscribed"}}`, message.idKey))
		return current.handleUpstreamText(ctx, response)
	}
	current = newTestSession(t, writer, nil)
	current.stateMu.Lock()
	current.effective["thr-a"] = "openai"
	current.stateMu.Unlock()

	status, err := current.Unsubscribe(context.Background(), "thr-a")
	if err != nil || status != handoff.StatusUnsubscribed {
		t.Fatalf("Unsubscribe() = %q, %v", status, err)
	}
	if got := current.effectiveProvider("thr-a"); got != "" {
		t.Fatalf("effective provider after unsubscribe = %q", got)
	}
	messages := upstream.messages()
	if len(messages) != 1 {
		t.Fatalf("upstream messages = %q", messages)
	}
	request, err := parseRPCMessage(messages[0])
	if err != nil || request.method != "thread/unsubscribe" || request.threadID != "thr-a" {
		t.Fatalf("unsubscribe request = %#v, %v", request, err)
	}
}

func TestSessionUnsubscribeClearsProviderWhenResponseIsUncertain(t *testing.T) {
	t.Parallel()
	var upstream messageRecorder
	current := newTestSession(t, upstream.write, nil)
	current.stateMu.Lock()
	current.effective["thr-a"] = "sub2api"
	current.stateMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := current.Unsubscribe(ctx, "thr-a"); err == nil {
		t.Fatal("Unsubscribe() unexpectedly succeeded")
	}
	if len(upstream.messages()) != 1 {
		t.Fatalf("upstream messages = %q", upstream.messages())
	}
	if got := current.effectiveProvider("thr-a"); got != "" {
		t.Fatalf("effective provider after uncertain unsubscribe = %q", got)
	}
}

func TestSessionResubscribesCoordinatorDetachedThread(t *testing.T) {
	t.Parallel()
	var current *session
	var upstream messageRecorder
	writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
		if err := upstream.write(ctx, messageType, payload); err != nil {
			return err
		}
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		switch message.method {
		case "thread/unsubscribe":
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"status":"unsubscribed"}}`, message.idKey)))
		case "thread/resume":
			var provider string
			_ = json.Unmarshal(message.params["modelProvider"], &provider)
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"thread":{"id":"thr-a"},"modelProvider":%q}}`, message.idKey, provider)))
		default:
			return nil
		}
	}
	current = newTestSession(t, writer, nil)
	current.stateMu.Lock()
	current.effective["thr-a"] = "openai"
	current.resumeTemplates["thr-a"] = map[string]json.RawMessage{
		"threadId": json.RawMessage(`"thr-a"`),
		"history":  json.RawMessage(`[{"type":"message"}]`),
		"path":     json.RawMessage(`"/stale/rollout.jsonl"`),
	}
	current.stateMu.Unlock()

	if _, err := current.Unsubscribe(context.Background(), "thr-a"); err != nil {
		t.Fatal(err)
	}
	status, err := current.Resubscribe(context.Background(), "thr-a", modelroute.Route{Provider: "sub2api"})
	if err != nil || status != handoff.StatusResubscribed {
		t.Fatalf("Resubscribe() = %q, %v", status, err)
	}
	if got := current.effectiveProvider("thr-a"); got != "sub2api" {
		t.Fatalf("effective provider after resubscribe = %q", got)
	}
	messages := upstream.messages()
	if len(messages) != 2 {
		t.Fatalf("upstream messages = %q", messages)
	}
	resume, err := parseRPCMessage(messages[1])
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resume.params["history"]; ok {
		t.Fatalf("resubscribe reused history: %#v", resume.params)
	}
	if _, ok := resume.params["path"]; ok {
		t.Fatalf("resubscribe reused stale path: %#v", resume.params)
	}
}

func TestSessionDoesNotResubscribeThreadThatPeerDidNotDetach(t *testing.T) {
	t.Parallel()
	var upstream messageRecorder
	current := newTestSession(t, upstream.write, nil)
	status, err := current.Resubscribe(context.Background(), "thr-a", modelroute.Route{Provider: "sub2api"})
	if err != nil || status != handoff.StatusNotSubscribed {
		t.Fatalf("Resubscribe() = %q, %v", status, err)
	}
	if len(upstream.messages()) != 0 {
		t.Fatalf("unexpected upstream resume: %q", upstream.messages())
	}
}

func TestSessionRestoreDetachedThreadAcceptsActualProvider(t *testing.T) {
	t.Parallel()
	var current *session
	var upstream messageRecorder
	writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
		if err := upstream.write(ctx, messageType, payload); err != nil {
			return err
		}
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		if _, ok := message.params["modelProvider"]; ok {
			return errors.New("restore requested a provider override")
		}
		return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
			`{"id":%s,"result":{"thread":{"id":"thr-a"},"modelProvider":"openai"}}`, message.idKey)))
	}
	current = newTestSession(t, writer, nil)
	current.setDetached("thr-a", true)

	status, err := current.Restore(context.Background(), "thr-a")
	if err != nil || status != handoff.StatusRestored {
		t.Fatalf("Restore() = %q, %v", status, err)
	}
	if current.isDetached("thr-a") {
		t.Fatal("restored thread remained detached")
	}
	if got := current.effectiveProvider("thr-a"); got != "openai" {
		t.Fatalf("effective provider after restore = %q", got)
	}
	if len(upstream.messages()) != 1 {
		t.Fatalf("upstream messages = %q", upstream.messages())
	}
}

func TestSessionSwitchesProviderBeforeTurnStart(t *testing.T) {
	t.Parallel()
	coordinator := &fakeHandoffCoordinator{}
	var current *session
	var upstream messageRecorder
	writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
		if err := upstream.write(ctx, messageType, payload); err != nil {
			return err
		}
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		if message.method == "thread/resume" && stringsHasPrefix(message.idKey, `"cps-`) {
			response := []byte(fmt.Sprintf(`{"id":%s,"result":{"thread":{"id":"thr-a"},"modelProvider":"sub2api"}}`, message.idKey))
			return current.handleUpstreamText(ctx, response)
		}
		return nil
	}
	current = newTestSession(t, writer, nil)
	current.coordinator = coordinator
	current.stateMu.Lock()
	current.effective["thr-a"] = "openai"
	current.resumeTemplates["thr-a"] = map[string]json.RawMessage{
		"threadId":      json.RawMessage(`"thr-a"`),
		"modelProvider": json.RawMessage(`"openai"`),
		"cwd":           json.RawMessage(`"/work"`),
	}
	current.stateMu.Unlock()

	request := []byte(`{"id":9,"method":"turn/start","params":{"threadId":"thr-a","input":[]}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if coordinator.lockCalls != 1 || coordinator.prepareCalls != 1 || coordinator.prepareHandoffCalls != 1 || coordinator.unsubscribeCalls != 1 ||
		coordinator.resubscribeCalls != 1 || coordinator.releaseCalls != 0 {
		t.Fatalf("coordinator calls = %#v", coordinator)
	}
	messages := upstream.messages()
	if len(messages) != 2 {
		t.Fatalf("upstream messages = %q", messages)
	}
	resume, err := parseRPCMessage(messages[0])
	if err != nil {
		t.Fatal(err)
	}
	var provider, cwd string
	_ = json.Unmarshal(resume.params["modelProvider"], &provider)
	_ = json.Unmarshal(resume.params["cwd"], &cwd)
	if resume.method != "thread/resume" || provider != "sub2api" || cwd != "/work" {
		t.Fatalf("internal resume = %#v", resume)
	}
	turn, err := parseRPCMessage(messages[1])
	if err != nil || turn.method != "turn/start" || turn.idKey != "9" {
		t.Fatalf("turn request = %#v, %v", turn, err)
	}
	if !current.isActive("thr-a") {
		t.Fatal("forwarded turn was not marked active")
	}
	if err := current.handleUpstreamText(context.Background(), []byte(
		`{"id":9,"result":{"turn":{"id":"turn-1","status":"inProgress"}}}`,
	)); err != nil {
		t.Fatal(err)
	}
	if coordinator.releaseCalls != 1 {
		t.Fatalf("turn/start acknowledgement release calls = %d, want 1", coordinator.releaseCalls)
	}
}

func TestSessionInternalResumeRewritesCollaborationModel(t *testing.T) {
	t.Parallel()
	var current *session
	var upstream messageRecorder
	writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
		if err := upstream.write(ctx, messageType, payload); err != nil {
			return err
		}
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
			`{"id":%s,"result":{"thread":{"id":"thr-a"},"modelProvider":"glm","model":"glm-5.2"}}`, message.idKey)))
	}
	current = newTestSession(t, writer, nil)
	current.stateMu.Lock()
	current.resumeTemplates["thr-a"] = map[string]json.RawMessage{
		"threadId":      json.RawMessage(`"thr-a"`),
		"modelProvider": json.RawMessage(`"openai"`),
		"model":         json.RawMessage(`"gpt-5.6-sol"`),
		"collaborationMode": json.RawMessage(
			`{"mode":"default","settings":{"model":"gpt-5.6-sol","effort":"high"}}`,
		),
	}
	current.stateMu.Unlock()

	wantRoute := modelroute.Route{Provider: "glm", Model: "glm-5.2"}
	if err := current.internalResumeWithRoute(context.Background(), "thr-a", wantRoute, true); err != nil {
		t.Fatal(err)
	}
	messages := upstream.messages()
	if len(messages) != 1 {
		t.Fatalf("internal resume messages = %q", messages)
	}
	resume, err := parseRPCMessage(messages[0])
	if err != nil {
		t.Fatal(err)
	}
	assertRawString(t, resume.params, "model", "glm-5.2")
	var collaboration struct {
		Settings struct {
			Model  string `json:"model"`
			Effort string `json:"effort"`
		} `json:"settings"`
	}
	if json.Unmarshal(resume.params["collaborationMode"], &collaboration) != nil ||
		collaboration.Settings.Model != "glm-5.2" || collaboration.Settings.Effort != "high" {
		t.Fatalf("internal resume collaborationMode = %s", resume.params["collaborationMode"])
	}
}

func TestSessionSanitizesRolloutBeforeResume(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dateDir := filepath.Join(home, "sessions", "2026", "08", "20")
	if err := os.MkdirAll(dateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dateDir, "rollout-2026-08-20T00-00-00-thr-a.jsonl")
	contents := `{"type":"session_meta","payload":{"id":"thr-a"}}` + "\n" + `{"type":"response_item","payload":{"type":"reasoning","id":"item_stale"}}` + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	var current *session
	writer := func(ctx context.Context, _ websocket.MessageType, payload []byte) error {
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		if message.method == "thread/list" {
			var providers []string
			if err := json.Unmarshal(message.params["modelProviders"], &providers); err != nil || providers == nil || len(providers) != 0 {
				t.Fatalf("thread/list modelProviders = %s, %v; want []", message.params["modelProviders"], err)
			}
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"data":[{"id":"thr-a","path":%q}],"nextCursor":null}}`, message.idKey, path)))
		}
		if message.method == "thread/read" {
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"thread":{"id":"thr-a","path":%q}}}`, message.idKey, path)))
		}
		return nil
	}
	current = newTestSession(t, writer, nil)
	current.codexHome = home
	if _, err := current.sanitizeThreadRollout(context.Background(), "thr-a", ""); err != nil {
		t.Fatalf("sanitizeThreadRollout() error = %v", err)
	}
	cleaned, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cleaned), "item_stale") {
		t.Fatalf("stale reasoning survived: %s", cleaned)
	}
}

func TestSessionHandoffSanitizesAfterUnsubscribe(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dateDir := filepath.Join(home, "sessions", "2026", "08", "20")
	lockDir := filepath.Join(home, "thread-writer-locks")
	if err := os.MkdirAll(dateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rolloutPath := filepath.Join(dateDir, "rollout-2026-08-20T00-00-00-thr-a.jsonl")
	contents := `{"type":"session_meta","payload":{"id":"thr-a"}}` + "\n" + `{"type":"response_item","payload":{"type":"reasoning","id":"item_handoff_stale"}}` + "\n"
	if err := os.WriteFile(rolloutPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(lockDir, "thr-a.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	coordinator := &fakeHandoffCoordinator{unsubscribeHook: func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	}}
	current := newResponsiveResumeSession(t, coordinator, "sub2api", rolloutPath)
	current.codexHome = home
	if err := current.handoff(context.Background(), "thr-a", modelroute.Route{Provider: "sub2api"}); err != nil {
		t.Fatalf("handoff() error = %v", err)
	}
	cleaned, err := os.ReadFile(rolloutPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cleaned), "item_handoff_stale") {
		t.Fatalf("handoff resumed before sanitation: %s", cleaned)
	}
}

func TestSessionInternalResumeRejectsMalformedCollaborationBeforeWrite(t *testing.T) {
	t.Parallel()
	var upstream messageRecorder
	current := newTestSession(t, upstream.write, nil)
	current.stateMu.Lock()
	current.resumeTemplates["thr-a"] = map[string]json.RawMessage{
		"threadId":          json.RawMessage(`"thr-a"`),
		"collaborationMode": json.RawMessage(`"unsafe-value"`),
	}
	current.stateMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := current.internalResumeWithRoute(
		ctx, "thr-a", modelroute.Route{Provider: "glm", Model: "glm-5.2"}, true,
	)
	if err == nil {
		t.Fatal("internalResumeWithRoute() error = nil")
	}
	if bytes.Contains([]byte(err.Error()), []byte("unsafe-value")) {
		t.Fatalf("internal resume error leaked collaboration value: %v", err)
	}
	if len(upstream.messages()) != 0 {
		t.Fatalf("malformed internal resume reached app-server: %q", upstream.messages())
	}
}

func TestSessionChecksPeersBeforeSameProviderTurn(t *testing.T) {
	t.Parallel()
	coordinator := &fakeHandoffCoordinator{}
	var upstream messageRecorder
	current := newTestSession(t, upstream.write, nil)
	current.coordinator = coordinator
	current.stateMu.Lock()
	current.effective["thr-a"] = "sub2api"
	current.stateMu.Unlock()

	request := []byte(`{"id":11,"method":"turn/start","params":{"threadId":"thr-a","input":[]}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if coordinator.prepareCalls != 1 || coordinator.unsubscribeCalls != 0 || coordinator.resubscribeCalls != 0 {
		t.Fatalf("coordinator calls = %#v", coordinator)
	}
	if len(upstream.messages()) != 1 {
		t.Fatalf("upstream messages = %q", upstream.messages())
	}
}

func TestSessionReconcilesStalePeerBusyFromAuthoritativeQuiescence(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"idle", "systemError"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			coordinator := &fakeHandoffCoordinator{prepareErr: errors.New("stale peer busy")}
			var current *session
			var upstream, downstream messageRecorder
			writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
				if err := upstream.write(ctx, messageType, payload); err != nil {
					return err
				}
				message, err := parseRPCMessage(payload)
				if err != nil {
					return err
				}
				if message.method == "thread/list" {
					return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
						`{"id":%s,"result":{"data":[{"id":"thr-a","path":null,"status":{"type":%q},"modelProvider":"sub2api"}],"nextCursor":null}}`,
						message.idKey, status)))
				}
				return nil
			}
			current = newTestSession(t, writer, downstream.write)
			current.coordinator = coordinator
			current.stateMu.Lock()
			current.effective["thr-a"] = "sub2api"
			current.stateMu.Unlock()

			request := []byte(`{"id":12,"method":"turn/start","params":{"threadId":"thr-a","input":[]}}`)
			if err := current.handleDownstreamText(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			if coordinator.reconcileCalls != 1 || coordinator.prepareCalls != 2 {
				t.Fatalf("coordinator calls = %#v, want one reconcile and two prepares", coordinator)
			}
			messages := upstream.messages()
			if len(messages) != 2 || !bytes.Equal(messages[1], request) {
				t.Fatalf("upstream messages = %q, want status probe then ordinary turn", messages)
			}
			if got := downstream.messages(); len(got) != 0 {
				t.Fatalf("quiescent reconciliation wrote error = %q", got)
			}
		})
	}
}

func TestSessionReturnsStaticErrorWhenHandoffFails(t *testing.T) {
	t.Parallel()
	coordinator := &fakeHandoffCoordinator{prepareErr: errors.New("secret peer path")}
	var upstream, downstream messageRecorder
	var current *session
	writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
		if err := upstream.write(ctx, messageType, payload); err != nil {
			return err
		}
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		if message.method == "thread/list" {
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"data":[{"id":"thr-a","path":null,"status":{"type":"active"},"modelProvider":"openai"}],"nextCursor":null}}`,
				message.idKey)))
		}
		return nil
	}
	current = newTestSession(t, writer, downstream.write)
	current.coordinator = coordinator
	current.stateMu.Lock()
	current.effective["thr-a"] = "openai"
	current.stateMu.Unlock()

	request := []byte(`{"id":"desktop-secret-id","method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"secret prompt"}]}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	upstreamMessages := upstream.messages()
	if len(upstreamMessages) != 1 {
		t.Fatalf("failed handoff upstream messages = %q, want one status probe", upstreamMessages)
	}
	probe, err := parseRPCMessage(upstreamMessages[0])
	if err != nil || probe.method != "thread/list" || bytes.Contains(upstreamMessages[0], []byte("secret prompt")) {
		t.Fatalf("failed handoff probe = %q, %v", upstreamMessages[0], err)
	}
	if coordinator.reconcileCalls != 0 {
		t.Fatalf("active app-server state reconciled %d peers", coordinator.reconcileCalls)
	}
	messages := downstream.messages()
	if len(messages) != 1 {
		t.Fatalf("downstream messages = %q", messages)
	}
	var response struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(messages[0], &response); err != nil {
		t.Fatal(err)
	}
	if string(response.ID) != `"desktop-secret-id"` || response.Error.Code != -32090 || response.Error.Message != handoffUnavailableMessage {
		t.Fatalf("handoff error = %#v", response)
	}
	if bytes.Contains(messages[0], []byte("secret prompt")) || bytes.Contains(messages[0], []byte("secret peer")) {
		t.Fatalf("handoff error leaked details: %s", messages[0])
	}
}

func TestSessionBusyReconciliationFailsClosedWithoutVerifiedIdlePeers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name              string
		threadListResult  string
		reconcileErr      error
		wantReconcileCall int
	}{
		{
			name: "unknown app-server status",
			threadListResult: `{"result":{"data":[{"id":"thr-a","path":null,"status":{},` +
				`"modelProvider":"sub2api"}],"nextCursor":null}}`,
		},
		{
			name: "duplicate conflicting target rows",
			threadListResult: `{"result":{"data":[` +
				`{"id":"thr-a","path":null,"status":{"type":"idle"},"modelProvider":"sub2api"},` +
				`{"id":"thr-a","path":null,"status":{"type":"active"},"modelProvider":"sub2api"}` +
				`],"nextCursor":null}}`,
		},
		{
			name:             "app-server query error",
			threadListResult: `{"error":{"code":-32603,"message":"unavailable"}}`,
		},
		{
			name: "peer reconciliation error",
			threadListResult: `{"result":{"data":[{"id":"thr-a","path":null,"status":{"type":"idle"},` +
				`"modelProvider":"sub2api"}],"nextCursor":null}}`,
			reconcileErr:      errors.New("peer unavailable"),
			wantReconcileCall: 1,
		},
		{
			name: "peer remains busy after reconciliation",
			threadListResult: `{"result":{"data":[{"id":"thr-a","path":null,"status":{"type":"idle"},` +
				`"modelProvider":"sub2api"}],"nextCursor":null}}`,
			wantReconcileCall: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			coordinator := &fakeHandoffCoordinator{
				prepareErr: errors.New("peer busy"), reconcileErr: tt.reconcileErr,
			}
			if tt.name == "peer remains busy after reconciliation" {
				coordinator.prepareAfterReconcileErr = errors.New("peer still busy")
			}
			var current *session
			var upstream, downstream messageRecorder
			writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
				if err := upstream.write(ctx, messageType, payload); err != nil {
					return err
				}
				message, err := parseRPCMessage(payload)
				if err != nil {
					return err
				}
				if message.method == "thread/list" {
					return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
						`{"id":%s,%s`, message.idKey, strings.TrimPrefix(tt.threadListResult, "{"))))
				}
				return nil
			}
			current = newTestSession(t, writer, downstream.write)
			current.coordinator = coordinator
			current.stateMu.Lock()
			current.effective["thr-a"] = "sub2api"
			current.stateMu.Unlock()

			request := []byte(`{"id":14,"method":"turn/start","params":{"threadId":"thr-a","input":[]}}`)
			if err := current.handleDownstreamText(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			if coordinator.reconcileCalls != tt.wantReconcileCall {
				t.Fatalf("reconcile calls = %d, want %d", coordinator.reconcileCalls, tt.wantReconcileCall)
			}
			if messages := upstream.messages(); len(messages) != 1 {
				t.Fatalf("upstream messages = %q, want only status probe", messages)
			}
			if messages := downstream.messages(); len(messages) != 1 || !bytes.Contains(messages[0], []byte(`"code":-32090`)) {
				t.Fatalf("downstream messages = %q, want static handoff error", messages)
			}
		})
	}
}

func TestSessionRejectsLegacyPeerBeforeUnsubscribe(t *testing.T) {
	t.Parallel()
	coordinator := &fakeHandoffCoordinator{
		prepareHandoffErr: errors.New("legacy peer"),
		unsubscribeErr:    errors.New("must not be called"),
	}
	var upstream, downstream messageRecorder
	current := newTestSession(t, upstream.write, downstream.write)
	current.coordinator = coordinator
	current.stateMu.Lock()
	current.effective["thr-a"] = "openai"
	current.stateMu.Unlock()

	request := []byte(`{"id":14,"method":"turn/start","params":{"threadId":"thr-a","input":[]}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if coordinator.prepareHandoffCalls != 1 || coordinator.unsubscribeCalls != 0 {
		t.Fatalf("coordinator calls = %#v", coordinator)
	}
	if len(upstream.messages()) != 0 || len(downstream.messages()) != 1 {
		t.Fatalf("upstream = %q, downstream = %q", upstream.messages(), downstream.messages())
	}
}

func TestSessionRetriesFullHandoffAfterResubscribeFailure(t *testing.T) {
	t.Parallel()
	coordinator := &fakeHandoffCoordinator{resubscribeErr: errors.New("peer unavailable")}
	var current *session
	var upstream, downstream messageRecorder
	writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
		if err := upstream.write(ctx, messageType, payload); err != nil {
			return err
		}
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		if message.method == "thread/resume" {
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"thread":{"id":"thr-a"},"modelProvider":"sub2api"}}`, message.idKey)))
		}
		return nil
	}
	current = newTestSession(t, writer, downstream.write)
	current.coordinator = coordinator
	current.stateMu.Lock()
	current.effective["thr-a"] = "openai"
	current.stateMu.Unlock()

	first := []byte(`{"id":12,"method":"turn/start","params":{"threadId":"thr-a","input":[]}}`)
	if err := current.handleDownstreamText(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if got := current.effectiveProvider("thr-a"); got != "" {
		t.Fatalf("effective provider after failed resubscribe = %q", got)
	}
	coordinator.resubscribeErr = nil
	second := []byte(`{"id":13,"method":"turn/start","params":{"threadId":"thr-a","input":[]}}`)
	if err := current.handleDownstreamText(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if coordinator.unsubscribeCalls != 2 || coordinator.resubscribeCalls != 2 {
		t.Fatalf("coordinator calls = %#v", coordinator)
	}
	if messages := upstream.messages(); len(messages) != 3 {
		t.Fatalf("upstream messages = %q", messages)
	}
}

func TestSessionRestoresPeersWhenSenderResumeFails(t *testing.T) {
	t.Parallel()
	coordinator := &fakeHandoffCoordinator{}
	var current *session
	var upstream, downstream messageRecorder
	writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
		if err := upstream.write(ctx, messageType, payload); err != nil {
			return err
		}
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		if message.method == "thread/resume" {
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"thread":{"id":"thr-a"},"modelProvider":"openai"}}`, message.idKey)))
		}
		return nil
	}
	current = newTestSession(t, writer, downstream.write)
	current.coordinator = coordinator
	current.stateMu.Lock()
	current.effective["thr-a"] = "openai"
	current.stateMu.Unlock()

	request := []byte(`{"id":15,"method":"turn/start","params":{"threadId":"thr-a","input":[]}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if coordinator.markDirtyCalls != 1 || coordinator.restoreCalls != 1 || coordinator.clearDirtyCalls != 0 || !coordinator.dirty {
		t.Fatalf("coordinator recovery state = %#v", coordinator)
	}
	if len(upstream.messages()) != 1 || len(downstream.messages()) != 1 {
		t.Fatalf("upstream = %q, downstream = %q", upstream.messages(), downstream.messages())
	}
}

func TestSessionDirtyStateForcesDifferentPeerToRepairPartialResubscribe(t *testing.T) {
	t.Parallel()
	coordinator := &fakeHandoffCoordinator{resubscribeErr: errors.New("partial peer failure")}
	first := newResponsiveResumeSession(t, coordinator, "sub2api")
	first.stateMu.Lock()
	first.effective["thr-a"] = "openai"
	first.stateMu.Unlock()

	firstRequest := []byte(`{"id":16,"method":"turn/start","params":{"threadId":"thr-a","input":[]}}`)
	if err := first.handleDownstreamText(context.Background(), firstRequest); err != nil {
		t.Fatal(err)
	}
	if !coordinator.dirty {
		t.Fatal("partial resubscribe did not leave global dirty state")
	}
	if got := coordinator.dirtyStageCalls[len(coordinator.dirtyStageCalls)-1]; got != handoff.DirtyStageResubscribing {
		t.Fatalf("partial resubscribe dirty stage = %q, want %q", got, handoff.DirtyStageResubscribing)
	}

	coordinator.resubscribeErr = nil
	second := newResponsiveResumeSession(t, coordinator, "sub2api")
	second.stateMu.Lock()
	second.effective["thr-a"] = "sub2api"
	second.stateMu.Unlock()
	secondRequest := []byte(`{"id":17,"method":"turn/start","params":{"threadId":"thr-a","input":[]}}`)
	if err := second.handleDownstreamText(context.Background(), secondRequest); err != nil {
		t.Fatal(err)
	}
	if coordinator.unsubscribeCalls != 2 || coordinator.resubscribeCalls != 2 ||
		coordinator.restoreCalls != 1 || coordinator.clearDirtyCalls != 1 || coordinator.dirty {
		t.Fatalf("coordinator recovery state = %#v", coordinator)
	}
}

func newResponsiveResumeSession(t *testing.T, coordinator handoffCoordinator, provider string, rolloutPaths ...string) *session {
	t.Helper()
	var current *session
	writer := func(ctx context.Context, _ websocket.MessageType, payload []byte) error {
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		path := ""
		if len(rolloutPaths) != 0 {
			path = rolloutPaths[0]
		}
		if message.method == "thread/list" {
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"data":[{"id":"thr-a","path":%q}],"nextCursor":null}}`, message.idKey, path)))
		}
		if message.method == "thread/read" {
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"thread":{"id":"thr-a","path":%q}}}`, message.idKey, path)))
		}
		if message.method == "thread/resume" {
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"thread":{"id":"thr-a"},"modelProvider":%q}}`, message.idKey, provider)))
		}
		return nil
	}
	current = newTestSession(t, writer, nil)
	current.coordinator = coordinator
	return current
}

func TestSessionRejectsDuplicateTurnStartWithoutClearingActiveState(t *testing.T) {
	t.Parallel()
	coordinator := &fakeHandoffCoordinator{}
	var upstream, downstream messageRecorder
	current := newTestSession(t, upstream.write, downstream.write)
	current.coordinator = coordinator
	current.stateMu.Lock()
	current.effective["thr-a"] = "sub2api"
	current.active["thr-a"] = true
	current.stateMu.Unlock()

	request := []byte(`{"id":10,"method":"turn/start","params":{"threadId":"thr-a","input":[]}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(upstream.messages()) != 0 {
		t.Fatalf("duplicate turn was forwarded upstream: %q", upstream.messages())
	}
	if len(downstream.messages()) != 1 {
		t.Fatalf("downstream messages = %q", downstream.messages())
	}
	if !current.isActive("thr-a") {
		t.Fatal("duplicate turn cleared active state")
	}
	if status := current.Prepare("thr-a"); status != handoff.StatusBusy {
		t.Fatalf("Prepare() after duplicate turn = %q", status)
	}
}

func TestSessionPendingTurnFenceReleasesOnConnectionClose(t *testing.T) {
	t.Parallel()
	coordinator := &fakeHandoffCoordinator{}
	var upstream messageRecorder
	current := newTestSession(t, upstream.write, nil)
	current.coordinator = coordinator
	current.stateMu.Lock()
	current.effective["thr-a"] = "sub2api"
	current.stateMu.Unlock()

	request := []byte(`{"id":10,"method":"turn/start","params":{"threadId":"thr-a","input":[]}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if coordinator.releaseCalls != 0 {
		t.Fatalf("pending turn released fence %d times", coordinator.releaseCalls)
	}
	current.closeState()
	if coordinator.releaseCalls != 1 {
		t.Fatalf("connection close released fence %d times, want 1", coordinator.releaseCalls)
	}
}

func TestSessionTurnWriteFailureReleasesFenceAndActiveState(t *testing.T) {
	t.Parallel()
	coordinator := &fakeHandoffCoordinator{}
	current := newTestSession(t, func(context.Context, websocket.MessageType, []byte) error {
		return errors.New("write failed")
	}, nil)
	current.coordinator = coordinator
	current.stateMu.Lock()
	current.effective["thr-a"] = "sub2api"
	current.stateMu.Unlock()

	request := []byte(`{"id":10,"method":"turn/start","params":{"threadId":"thr-a","input":[]}}`)
	if err := current.handleDownstreamText(context.Background(), request); err == nil {
		t.Fatal("turn write error = nil")
	}
	if coordinator.releaseCalls != 1 {
		t.Fatalf("write failure released fence %d times, want 1", coordinator.releaseCalls)
	}
	if current.isActive("thr-a") {
		t.Fatal("write failure left thread active")
	}
}

func TestSessionRejectsCrossThreadDuplicateRequestIDWithoutReplacingTurnFence(t *testing.T) {
	t.Parallel()
	coordinator := &fakeHandoffCoordinator{}
	var upstream, downstream messageRecorder
	current := newTestSession(t, upstream.write, downstream.write)
	current.coordinator = coordinator
	current.stateMu.Lock()
	current.effective["thr-a"] = "sub2api"
	current.effective["thr-b"] = "sub2api"
	current.stateMu.Unlock()

	first := []byte(`{"id":10,"method":"turn/start","params":{"threadId":"thr-a","input":[]}}`)
	second := []byte(`{"id":10,"method":"turn/start","params":{"threadId":"thr-b","input":[]}}`)
	if err := current.handleDownstreamText(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := current.handleDownstreamText(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if messages := upstream.messages(); len(messages) != 1 || !bytes.Equal(messages[0], first) {
		t.Fatalf("duplicate request reached upstream: %q", messages)
	}
	if messages := downstream.messages(); len(messages) != 1 || !bytes.Contains(messages[0], []byte(`"code":-32090`)) {
		t.Fatalf("duplicate request response = %q", messages)
	}
	if coordinator.lockCalls != 1 || coordinator.releaseCalls != 0 {
		t.Fatalf("duplicate request changed turn fence: %#v", coordinator)
	}
	if !current.isActive("thr-a") || current.isActive("thr-b") {
		t.Fatalf("duplicate request changed active state: thr-a=%v thr-b=%v", current.isActive("thr-a"), current.isActive("thr-b"))
	}
}

func TestSessionReservesTransparentRequestIDBeforeTrackedTurn(t *testing.T) {
	t.Parallel()
	coordinator := &fakeHandoffCoordinator{}
	var upstream, downstream messageRecorder
	current := newTestSession(t, upstream.write, downstream.write)
	current.coordinator = coordinator
	current.stateMu.Lock()
	current.effective["thr-a"] = "sub2api"
	current.stateMu.Unlock()

	transparent := []byte(`{"id":10,"method":"model/list","params":{}}`)
	turn := []byte(`{"id":10,"method":"turn/start","params":{"threadId":"thr-a","input":[]}}`)
	if err := current.handleDownstreamText(context.Background(), transparent); err != nil {
		t.Fatal(err)
	}
	if err := current.handleDownstreamText(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	if messages := upstream.messages(); len(messages) != 1 || !bytes.Equal(messages[0], transparent) {
		t.Fatalf("request-id reuse reached upstream: %q", messages)
	}
	if messages := downstream.messages(); len(messages) != 1 || !bytes.Contains(messages[0], []byte(`"code":-32090`)) {
		t.Fatalf("request-id reuse response = %q", messages)
	}
	if coordinator.lockCalls != 0 || coordinator.releaseCalls != 0 || current.isActive("thr-a") {
		t.Fatalf("request-id reuse changed turn state: %#v", coordinator)
	}
}

func TestSessionProviderStatusReportsRuntimeAndSelectionWithoutMutation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		runtime   string
		selection map[string]string
		want      string
	}{
		{
			name:      "verified runtime matches selection",
			runtime:   "sub2api",
			selection: map[string]string{"thr-a": "sub2api"},
			want:      "Runtime provider: sub2api (verified).\nSelected provider: sub2api.",
		},
		{
			name:      "verified runtime differs from selection",
			runtime:   "openai",
			selection: map[string]string{"thr-a": "sub2api"},
			want:      "Runtime provider: openai (verified).\nSelected provider: sub2api (will be applied before the next model turn).",
		},
		{
			name:      "runtime unknown with selection",
			selection: map[string]string{"thr-a": "sub2api"},
			want:      "Runtime provider: unknown.\nSelected provider: sub2api (will be applied before the next model turn).",
		},
		{
			name:      "invalid runtime is not reported as verified",
			runtime:   "invalid/provider",
			selection: map[string]string{"thr-a": "sub2api"},
			want:      "Runtime provider: unknown.\nSelected provider: sub2api (will be applied before the next model turn).",
		},
		{
			name:      "runtime and selection unknown",
			selection: map[string]string{},
			want:      "Runtime provider: unknown.\nSelected provider: app-server configuration.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			coordinator := &fakeHandoffCoordinator{}
			selections := &fakeProviderSelections{values: tt.selection}
			var upstream, downstream messageRecorder
			current := newTestSession(t, upstream.write, downstream.write)
			current.provider = ""
			current.selections = selections
			current.coordinator = coordinator
			if tt.runtime != "" {
				current.stateMu.Lock()
				current.effective["thr-a"] = tt.runtime
				current.stateMu.Unlock()
			}

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			request := []byte(`{"id":20,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"/provider status"}]}}`)
			if err := current.handleDownstreamText(ctx, request); err != nil {
				t.Fatal(err)
			}

			if got := upstream.messages(); len(got) != 0 {
				t.Fatalf("status wrote to app-server: %q", got)
			}
			if got := syntheticAgentFeedback(t, downstream.messages()); got != tt.want {
				t.Fatalf("status feedback = %q, want %q", got, tt.want)
			}
			if selections.setCalls != 0 {
				t.Fatalf("status persisted selection %d times", selections.setCalls)
			}
			if coordinator.prepareCalls != 1 || coordinator.prepareHandoffCalls != 0 ||
				coordinator.unsubscribeCalls != 0 || coordinator.resubscribeCalls != 0 ||
				coordinator.markDirtyCalls != 0 || coordinator.clearDirtyCalls != 0 {
				t.Fatalf("status coordinator calls = %#v", coordinator)
			}
		})
	}
}

func TestSessionMappedProviderStatusReportsVerifiedModel(t *testing.T) {
	t.Parallel()
	var upstream, downstream messageRecorder
	current := newTestSession(t, upstream.write, downstream.write)
	current.provider = ""
	current.routes = testModelCatalog(t)
	current.selections = &fakeProviderSelections{values: map[string]string{"thr-a": "glm"}}
	current.coordinator = &fakeHandoffCoordinator{}
	current.stateMu.Lock()
	current.effective["thr-a"] = "glm"
	current.effectiveModel["thr-a"] = "glm-5.2"
	current.stateMu.Unlock()

	request := []byte(`{"id":120,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"/provider status"}]}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	want := "Runtime provider: glm (verified).\nRuntime model: glm-5.2 (verified).\nSelected provider: glm.\nSelected model: glm-5.2."
	if got := syntheticAgentFeedback(t, downstream.messages()); got != want {
		t.Fatalf("status feedback = %q, want %q", got, want)
	}
	if len(upstream.messages()) != 0 {
		t.Fatalf("status reached app-server: %q", upstream.messages())
	}
}

func TestSessionAppliesMappedRouteToInitialThreadAndNormalTurn(t *testing.T) {
	t.Parallel()
	var upstream messageRecorder
	current := newTestSession(t, upstream.write, nil)
	current.provider = "glm"
	current.routes = testModelCatalog(t)

	start := []byte(`{"id":121,"method":"thread/start","params":{"modelProvider":"openai","model":"gpt-5.6-sol","cwd":"/tmp"}}`)
	if err := current.handleDownstreamText(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	startMessage, err := parseRPCMessage(upstream.messages()[0])
	if err != nil {
		t.Fatal(err)
	}
	assertRawString(t, startMessage.params, "modelProvider", "glm")
	assertRawString(t, startMessage.params, "model", "glm-5.2")

	upstream = messageRecorder{}
	current.provider = ""
	current.selections = &fakeProviderSelections{values: map[string]string{"thr-a": "glm"}}
	current.coordinator = &fakeHandoffCoordinator{}
	current.stateMu.Lock()
	current.effective["thr-a"] = "glm"
	current.effectiveModel["thr-a"] = "glm-5.2"
	current.stateMu.Unlock()
	turn := []byte(`{"id":122,"method":"turn/start","params":{"threadId":"thr-a","model":"gpt-5.6-sol","input":[{"type":"text","text":"keep me"}]}}`)
	if err := current.handleDownstreamText(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	turnMessage, err := parseRPCMessage(upstream.messages()[0])
	if err != nil {
		t.Fatal(err)
	}
	assertRawString(t, turnMessage.params, "model", "glm-5.2")
	if _, present := turnMessage.params["modelProvider"]; present {
		t.Fatalf("turn/start gained modelProvider: %s", upstream.messages()[0])
	}
	var input []commandInputItem
	if json.Unmarshal(turnMessage.params["input"], &input) != nil || len(input) != 1 || input[0].Text != "keep me" {
		t.Fatalf("turn input changed: %s", turnMessage.params["input"])
	}
}

func TestSessionAppliesSavedRouteToThreadSettingsUpdate(t *testing.T) {
	t.Parallel()
	var upstream messageRecorder
	coordinator := &fakeHandoffCoordinator{}
	current := newTestSession(t, upstream.write, nil)
	current.provider = ""
	current.routes = testModelCatalog(t)
	current.selections = &fakeProviderSelections{values: map[string]string{"thr-a": "glm"}}
	current.coordinator = coordinator

	request := []byte(`{"id":123,"method":"thread/settings/update","params":{"threadId":"thr-a","model":"gpt-5.6-sol","collaborationMode":{"mode":"default","settings":{"model":"gpt-5.6-sol","effort":"high"}}}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	messages := upstream.messages()
	if len(messages) != 1 {
		t.Fatalf("upstream messages = %q, want one settings update", messages)
	}
	message, err := parseRPCMessage(messages[0])
	if err != nil {
		t.Fatal(err)
	}
	assertRawString(t, message.params, "model", "glm-5.2")
	if _, present := message.params["modelProvider"]; present {
		t.Fatalf("settings update gained modelProvider: %s", messages[0])
	}
	var collaboration struct {
		Settings struct {
			Model  string `json:"model"`
			Effort string `json:"effort"`
		} `json:"settings"`
	}
	if json.Unmarshal(message.params["collaborationMode"], &collaboration) != nil ||
		collaboration.Settings.Model != "glm-5.2" || collaboration.Settings.Effort != "high" {
		t.Fatalf("collaborationMode = %s", message.params["collaborationMode"])
	}
	if coordinator.lockCalls != 1 || coordinator.releaseCalls != 1 {
		t.Fatalf("settings update was not serialized: %#v", coordinator)
	}
}

func TestSessionAcceptsAndPersistsAllowlistedModelFromSettingsUpdate(t *testing.T) {
	t.Parallel()
	var upstream messageRecorder
	var current *session
	writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
		if err := upstream.write(ctx, messageType, payload); err != nil {
			return err
		}
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(`{"id":%s,"result":{}}`, message.idKey)))
	}
	selections := &fakeProviderSelections{
		values: map[string]string{"thr-a": "glm"},
		models: map[string]string{"thr-a": "glm-5.3"},
	}
	current = newTestSession(t, writer, nil)
	current.provider = ""
	current.routes = testMultiModelCatalog(t)
	current.selections = selections
	current.coordinator = &fakeHandoffCoordinator{}

	request := []byte(`{"id":223,"method":"thread/settings/update","params":{"threadId":"thr-a","model":"glm-5.2","collaborationMode":{"mode":"default","settings":{"model":"glm-5.2","effort":"high"}}}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	message, err := parseRPCMessage(upstream.messages()[0])
	if err != nil {
		t.Fatal(err)
	}
	assertRawString(t, message.params, "model", "glm-5.2")
	if got := selections.models["thr-a"]; got != "glm-5.2" {
		t.Fatalf("persisted model = %q, want glm-5.2", got)
	}
}

func TestSessionPersistsSettingsModelOnlyAfterSuccessfulResponse(t *testing.T) {
	t.Parallel()
	requestSeen := make(chan rpcMessage, 1)
	selections := &fakeProviderSelections{
		values: map[string]string{"thr-a": "glm"},
		models: map[string]string{"thr-a": "glm-5.3"},
	}
	var current *session
	writer := func(_ context.Context, _ websocket.MessageType, payload []byte) error {
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		requestSeen <- message
		return nil
	}
	current = newTestSession(t, writer, nil)
	current.provider = ""
	current.routes = testMultiModelCatalog(t)
	current.selections = selections
	current.coordinator = &fakeHandoffCoordinator{}

	done := make(chan error, 1)
	request := []byte(`{"id":228,"method":"thread/settings/update","params":{"threadId":"thr-a","model":"glm-5.2"}}`)
	go func() { done <- current.handleDownstreamText(context.Background(), request) }()
	message := <-requestSeen
	if got := selections.models["thr-a"]; got != "glm-5.3" {
		t.Fatalf("model persisted before app-server response: %q", got)
	}
	select {
	case err := <-done:
		t.Fatalf("settings handler returned before app-server response: %v", err)
	default:
	}
	if err := current.handleUpstreamText(context.Background(), []byte(fmt.Sprintf(`{"id":%s,"result":{}}`, message.idKey))); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := selections.models["thr-a"]; got != "glm-5.2" {
		t.Fatalf("model after successful response = %q, want glm-5.2", got)
	}
}

func TestSessionDoesNotPersistSettingsModelAfterAppServerError(t *testing.T) {
	t.Parallel()
	selections := &fakeProviderSelections{
		values: map[string]string{"thr-a": "glm"},
		models: map[string]string{"thr-a": "glm-5.3"},
	}
	var current *session
	writer := func(ctx context.Context, _ websocket.MessageType, payload []byte) error {
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
			`{"id":%s,"error":{"code":-32602,"message":"rejected"}}`, message.idKey)))
	}
	current = newTestSession(t, writer, nil)
	current.provider = ""
	current.routes = testMultiModelCatalog(t)
	current.selections = selections
	current.coordinator = &fakeHandoffCoordinator{}

	request := []byte(`{"id":229,"method":"thread/settings/update","params":{"threadId":"thr-a","model":"glm-5.2"}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if got := selections.models["thr-a"]; got != "glm-5.3" {
		t.Fatalf("rejected settings update persisted model %q", got)
	}
}

func TestSessionDoesNotPersistSettingsModelWhenUpstreamWriteFails(t *testing.T) {
	t.Parallel()
	selections := &fakeProviderSelections{
		values: map[string]string{"thr-a": "glm"},
		models: map[string]string{"thr-a": "glm-5.3"},
	}
	current := newTestSession(t, func(context.Context, websocket.MessageType, []byte) error {
		return errors.New("write failed")
	}, nil)
	current.provider = ""
	current.routes = testMultiModelCatalog(t)
	current.selections = selections
	current.coordinator = &fakeHandoffCoordinator{}

	request := []byte(`{"id":230,"method":"thread/settings/update","params":{"threadId":"thr-a","model":"glm-5.2"}}`)
	if err := current.handleDownstreamText(context.Background(), request); err == nil {
		t.Fatal("handleDownstreamText() error = nil")
	}
	if got := selections.models["thr-a"]; got != "glm-5.3" {
		t.Fatalf("failed write persisted model %q", got)
	}
}

func TestSessionUsesPersistedAllowlistedModelForNextTurn(t *testing.T) {
	t.Parallel()
	current := newTestSession(t, nil, nil)
	current.provider = ""
	current.routes = testMultiModelCatalog(t)
	current.selections = &fakeProviderSelections{
		values: map[string]string{"thr-a": "glm"},
		models: map[string]string{"thr-a": "glm-5.2"},
	}

	got, selected, err := current.selectedRoute("thr-a")
	want := modelroute.Route{Provider: "glm", Model: "glm-5.2"}
	if err != nil || !selected || got != want {
		t.Fatalf("selectedRoute() = %#v, %v, %v; want %#v", got, selected, err, want)
	}
}

func TestSessionIgnoresStaleTurnModelWhenExactSelectionExists(t *testing.T) {
	t.Parallel()
	var upstream messageRecorder
	var current *session
	writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
		if err := upstream.write(ctx, messageType, payload); err != nil {
			return err
		}
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		if message.method == "thread/resume" {
			var provider, model string
			_ = json.Unmarshal(message.params["modelProvider"], &provider)
			_ = json.Unmarshal(message.params["model"], &model)
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"thread":{"id":"thr-a"},"modelProvider":%q,"model":%q}}`,
				message.idKey, provider, model)))
		}
		return nil
	}
	selections := &fakeProviderSelections{
		values: map[string]string{"thr-a": "glm"},
		models: map[string]string{"thr-a": "glm-5.3"},
	}
	current = newTestSession(t, writer, nil)
	current.provider = ""
	current.routes = testMultiModelCatalog(t)
	current.selections = selections
	current.coordinator = &fakeHandoffCoordinator{}
	current.stateMu.Lock()
	current.effective["thr-a"] = "glm"
	current.effectiveModel["thr-a"] = "glm-5.2"
	current.stateMu.Unlock()

	request := []byte(`{"id":225,"method":"turn/start","params":{"threadId":"thr-a","model":"glm-5.2","input":[]}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	messages := upstream.messages()
	message, err := parseRPCMessage(messages[len(messages)-1])
	if err != nil {
		t.Fatal(err)
	}
	assertRawString(t, message.params, "model", "glm-5.3")
	if got := selections.models["thr-a"]; got != "glm-5.3" {
		t.Fatalf("stale turn changed persisted model to %q", got)
	}
	if coordinator := current.coordinator.(*fakeHandoffCoordinator); coordinator.prepareHandoffCalls != 0 ||
		coordinator.unsubscribeCalls != 0 || coordinator.resubscribeCalls != 0 ||
		coordinator.markDirtyCalls != 0 {
		t.Fatalf("same-provider model change triggered handoff: %#v", coordinator)
	}
}

func TestSessionAppliesSameProviderModelAfterTurnAcknowledgement(t *testing.T) {
	t.Parallel()
	coordinator := &fakeHandoffCoordinator{}
	selections := &fakeProviderSelections{
		values: map[string]string{"thr-a": "glm"},
		models: map[string]string{"thr-a": "glm-5.3"},
	}
	var current *session
	var upstream messageRecorder
	writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
		if err := upstream.write(ctx, messageType, payload); err != nil {
			return err
		}
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		if message.method == "thread/resume" {
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"thread":{"id":"thr-a"},"modelProvider":"glm","model":"glm-5.3"}}`,
				message.idKey)))
		}
		return nil
	}
	current = newTestSession(t, writer, nil)
	current.provider = ""
	current.routes = testMultiModelCatalog(t)
	current.selections = selections
	current.coordinator = coordinator
	current.stateMu.Lock()
	current.effective["thr-a"] = "glm"
	current.effectiveModel["thr-a"] = "glm-5.2"
	current.stateMu.Unlock()

	request := []byte(`{"id":232,"method":"turn/start","params":{"threadId":"thr-a","model":"glm-5.2","input":[]}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if got := current.effectiveRoute("thr-a"); got != (modelroute.Route{Provider: "glm", Model: "glm-5.2"}) {
		t.Fatalf("route changed before acknowledgement: %#v", got)
	}
	if coordinator.prepareHandoffCalls != 0 || coordinator.unsubscribeCalls != 0 ||
		coordinator.resubscribeCalls != 0 || coordinator.markDirtyCalls != 0 {
		t.Fatalf("same-provider model change triggered handoff: %#v", coordinator)
	}
	if got := upstream.messages(); len(got) != 1 {
		t.Fatalf("upstream messages = %q, want one turn/start", got)
	}
	message, err := parseRPCMessage(upstream.messages()[0])
	if err != nil {
		t.Fatal(err)
	}
	assertRawString(t, message.params, "model", "glm-5.3")

	if err := current.handleUpstreamText(context.Background(), []byte(
		`{"id":232,"result":{"turn":{"id":"turn-232","status":"inProgress"}}}`,
	)); err != nil {
		t.Fatal(err)
	}
	if got := current.effectiveRoute("thr-a"); got != (modelroute.Route{Provider: "glm", Model: "glm-5.3"}) {
		t.Fatalf("route after acknowledgement = %#v", got)
	}
}

func TestSessionRetainsVerifiedRouteAfterRejectedOrMalformedTurnResponse(t *testing.T) {
	t.Parallel()
	responses := map[string]string{
		"app-server error": `{"id":234,"error":{"code":-32602,"message":"rejected"}}`,
		"missing result":   `{"id":234}`,
		"scalar result":    `{"id":234,"result":"invalid"}`,
		"empty result":     `{"id":234,"result":{}}`,
		"empty turn id":    `{"id":234,"result":{"turn":{"id":""}}}`,
	}
	for name, response := range responses {
		name, response := name, response
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			coordinator := &fakeHandoffCoordinator{}
			current := newTestSession(t, nil, nil)
			current.provider = ""
			current.routes = testMultiModelCatalog(t)
			current.selections = &fakeProviderSelections{
				values: map[string]string{"thr-a": "glm"},
				models: map[string]string{"thr-a": "glm-5.3"},
			}
			current.coordinator = coordinator
			current.stateMu.Lock()
			current.effective["thr-a"] = "glm"
			current.effectiveModel["thr-a"] = "glm-5.2"
			current.stateMu.Unlock()

			request := []byte(`{"id":234,"method":"turn/start","params":{"threadId":"thr-a","input":[]}}`)
			if err := current.handleDownstreamText(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			if err := current.handleUpstreamText(context.Background(), []byte(response)); err != nil {
				t.Fatal(err)
			}
			if got := current.effectiveRoute("thr-a"); got != (modelroute.Route{Provider: "glm", Model: "glm-5.2"}) {
				t.Fatalf("rejected or malformed response changed route to %#v", got)
			}
			if current.isActive("thr-a") {
				t.Fatal("rejected or malformed response left turn active")
			}
		})
	}
}

func TestSessionRepairsSameProviderDirtyMarkerWithoutFullHandoff(t *testing.T) {
	t.Parallel()
	coordinator := &fakeHandoffCoordinator{dirty: true, dirtyStage: handoff.DirtyStageUnsubscribed}
	selections := &fakeProviderSelections{
		values: map[string]string{"thr-a": "glm"},
		models: map[string]string{"thr-a": "glm-5.3"},
	}
	var current *session
	var upstream messageRecorder
	writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
		if err := upstream.write(ctx, messageType, payload); err != nil {
			return err
		}
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		if message.method == "thread/resume" {
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"thread":{"id":"thr-a"},"modelProvider":"glm","model":"glm-5.3"}}`,
				message.idKey)))
		}
		return nil
	}
	current = newTestSession(t, writer, nil)
	current.provider = ""
	current.routes = testMultiModelCatalog(t)
	current.selections = selections
	current.coordinator = coordinator
	current.stateMu.Lock()
	current.effective["thr-a"] = "glm"
	current.effectiveModel["thr-a"] = "glm-5.2"
	current.stateMu.Unlock()

	request := []byte(`{"id":233,"method":"turn/start","params":{"threadId":"thr-a","input":[]}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if coordinator.prepareHandoffCalls != 1 || coordinator.resubscribeCalls != 1 ||
		coordinator.clearDirtyCalls != 1 {
		t.Fatalf("dirty repair calls = %#v", coordinator)
	}
	if coordinator.unsubscribeCalls != 0 || coordinator.markDirtyCalls != 0 || len(coordinator.dirtyStageCalls) != 0 {
		t.Fatalf("same-provider dirty repair started a new handoff: %#v", coordinator)
	}
	if coordinator.resubscribeRoute != (modelroute.Route{Provider: "glm"}) {
		t.Fatalf("dirty repair route = %#v, want provider-only glm route", coordinator.resubscribeRoute)
	}
	if coordinator.dirty {
		t.Fatal("same-provider dirty marker was not cleared")
	}
	if got := upstream.messages(); len(got) != 1 {
		t.Fatalf("upstream messages = %q, want one turn/start", got)
	}
	message, err := parseRPCMessage(upstream.messages()[0])
	if err != nil {
		t.Fatal(err)
	}
	assertRawString(t, message.params, "model", "glm-5.3")
}

func TestSessionUsesFullHandoffForOtherDirtyStages(t *testing.T) {
	t.Parallel()
	stages := []handoff.DirtyStage{
		handoff.DirtyStagePrepared,
		handoff.DirtyStageResumeMismatch,
		handoff.DirtyStageRecovering,
		handoff.DirtyStageResubscribing,
	}
	for _, stage := range stages {
		stage := stage
		t.Run(string(stage), func(t *testing.T) {
			t.Parallel()
			coordinator := &fakeHandoffCoordinator{dirty: true, dirtyStage: stage}
			current := newResponsiveResumeSession(t, coordinator, "sub2api")
			current.stateMu.Lock()
			current.effective["thr-a"] = "sub2api"
			current.stateMu.Unlock()

			request := []byte(`{"id":235,"method":"turn/start","params":{"threadId":"thr-a","input":[]}}`)
			if err := current.handleDownstreamText(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			if coordinator.unsubscribeCalls != 1 || coordinator.markDirtyCalls != 1 ||
				coordinator.resubscribeCalls != 1 || coordinator.clearDirtyCalls != 1 {
				t.Fatalf("dirty stage %q bypassed full handoff: %#v", stage, coordinator)
			}
		})
	}
}

func TestSessionRejectsForeignPickerModelForSelectedProvider(t *testing.T) {
	t.Parallel()
	var upstream messageRecorder
	selections := &fakeProviderSelections{
		values: map[string]string{"thr-a": "glm"},
	}
	current := newTestSession(t, upstream.write, nil)
	current.provider = ""
	current.routes = testMultiModelCatalog(t)
	current.selections = selections
	current.coordinator = &fakeHandoffCoordinator{}
	current.stateMu.Lock()
	current.effective["thr-a"] = "glm"
	current.effectiveModel["thr-a"] = "glm-5.3"
	current.stateMu.Unlock()

	request := []byte(`{"id":226,"method":"turn/start","params":{"threadId":"thr-a","model":"deepseek-v4-pro","input":[]}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	message, err := parseRPCMessage(upstream.messages()[0])
	if err != nil {
		t.Fatal(err)
	}
	assertRawString(t, message.params, "model", "glm-5.3")
	if got := selections.models["thr-a"]; got != "" {
		t.Fatalf("foreign model changed selection to %q", got)
	}
}

func TestRequestedModelPrecedenceAndValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		params  string
		want    string
		present bool
		wantErr bool
	}{
		{
			name:    "nested model wins",
			params:  `{"model":"glm-5.3","collaborationMode":{"settings":{"model":"glm-5.2"}}}`,
			want:    "glm-5.2",
			present: true,
		},
		{
			name:    "null top leaves nested model",
			params:  `{"model":null,"collaborationMode":{"settings":{"model":"glm-5.2"}}}`,
			want:    "glm-5.2",
			present: true,
		},
		{
			name:    "null nested falls back to top model",
			params:  `{"model":"glm-5.3","collaborationMode":{"settings":{"model":null}}}`,
			want:    "glm-5.3",
			present: true,
		},
		{
			name:   "both models null",
			params: `{"model":null,"collaborationMode":{"settings":{"model":null}}}`,
		},
		{
			name:    "null collaboration falls back to top model",
			params:  `{"model":"glm-5.3","collaborationMode":null}`,
			want:    "glm-5.3",
			present: true,
		},
		{
			name:    "invalid top model type",
			params:  `{"model":7,"collaborationMode":{"settings":{"model":"glm-5.2"}}}`,
			wantErr: true,
		},
		{
			name:    "invalid nested model type",
			params:  `{"model":"glm-5.3","collaborationMode":{"settings":{"model":false}}}`,
			wantErr: true,
		},
		{
			name:    "invalid collaboration shape",
			params:  `{"model":"glm-5.3","collaborationMode":"unsafe"}`,
			wantErr: true,
		},
		{
			name:    "missing collaboration settings",
			params:  `{"model":"glm-5.3","collaborationMode":{"mode":"default"}}`,
			wantErr: true,
		},
		{
			name:    "null collaboration settings",
			params:  `{"model":"glm-5.3","collaborationMode":{"settings":null}}`,
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var params map[string]json.RawMessage
			if err := json.Unmarshal([]byte(test.params), &params); err != nil {
				t.Fatal(err)
			}
			got, present, err := requestedModel(params)
			if (err != nil) != test.wantErr {
				t.Fatalf("requestedModel() error = %v, wantErr %v", err, test.wantErr)
			}
			if test.wantErr {
				return
			}
			if got != test.want || present != test.present {
				t.Fatalf("requestedModel() = %q, %v; want %q, %v", got, present, test.want, test.present)
			}
		})
	}
}

func TestSessionUsesNestedPickerModelWhenDesktopFieldsConflict(t *testing.T) {
	t.Parallel()
	var upstream messageRecorder
	current := newTestSession(t, upstream.write, nil)
	current.provider = ""
	current.routes = testMultiModelCatalog(t)
	current.selections = &fakeProviderSelections{values: map[string]string{"thr-a": "glm"}}
	current.coordinator = &fakeHandoffCoordinator{}
	current.stateMu.Lock()
	current.effective["thr-a"] = "glm"
	current.effectiveModel["thr-a"] = "glm-5.2"
	current.stateMu.Unlock()

	request := []byte(`{"id":227,"method":"turn/start","params":{"threadId":"thr-a","model":"glm-5.3","collaborationMode":{"mode":"default","settings":{"model":"glm-5.2"}},"input":[]}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	message, err := parseRPCMessage(upstream.messages()[0])
	if err != nil {
		t.Fatal(err)
	}
	assertRawString(t, message.params, "model", "glm-5.2")
	var collaboration struct {
		Settings struct {
			Model string `json:"model"`
		} `json:"settings"`
	}
	if json.Unmarshal(message.params["collaborationMode"], &collaboration) != nil ||
		collaboration.Settings.Model != "glm-5.2" {
		t.Fatalf("collaborationMode = %s", message.params["collaborationMode"])
	}
	if got := current.selections.(*fakeProviderSelections).models["thr-a"]; got != "glm-5.2" {
		t.Fatalf("persisted model = %q, want nested picker model", got)
	}
}

func TestSessionTreatsNullPickerModelsAsAbsent(t *testing.T) {
	t.Parallel()
	var upstream messageRecorder
	current := newTestSession(t, upstream.write, nil)
	current.provider = ""
	current.routes = testMultiModelCatalog(t)
	current.selections = &fakeProviderSelections{values: map[string]string{"thr-a": "glm"}}
	current.coordinator = &fakeHandoffCoordinator{}
	current.stateMu.Lock()
	current.effective["thr-a"] = "glm"
	current.effectiveModel["thr-a"] = "glm-5.3"
	current.stateMu.Unlock()

	request := []byte(`{"id":231,"method":"turn/start","params":{"threadId":"thr-a","model":null,"collaborationMode":{"mode":"default","settings":{"model":null}},"input":[]}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	message, err := parseRPCMessage(upstream.messages()[0])
	if err != nil {
		t.Fatal(err)
	}
	assertRawString(t, message.params, "model", "glm-5.3")
	var collaboration struct {
		Settings struct {
			Model string `json:"model"`
		} `json:"settings"`
	}
	if json.Unmarshal(message.params["collaborationMode"], &collaboration) != nil ||
		collaboration.Settings.Model != "glm-5.3" {
		t.Fatalf("collaborationMode = %s", message.params["collaborationMode"])
	}
}

func TestSessionPreservesUnselectedThreadSettingsUpdate(t *testing.T) {
	t.Parallel()
	var upstream messageRecorder
	coordinator := &fakeHandoffCoordinator{}
	current := newTestSession(t, upstream.write, nil)
	current.provider = ""
	current.routes = testModelCatalog(t)
	current.selections = &fakeProviderSelections{values: map[string]string{}}
	current.coordinator = coordinator

	request := []byte(` { "id": 124, "method": "thread/settings/update", "params": { "threadId": "thr-a", "model": "gpt-5.6-sol" } } `)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	messages := upstream.messages()
	if len(messages) != 1 || !bytes.Equal(messages[0], request) {
		t.Fatalf("unselected settings update = %q, want byte-for-byte %q", messages, request)
	}
	if coordinator.lockCalls != 1 || coordinator.releaseCalls != 1 {
		t.Fatalf("settings selection lookup was not serialized: %#v", coordinator)
	}
}

func TestSessionAppliesDirectOverrideToThreadSettingsUpdate(t *testing.T) {
	t.Parallel()
	var upstream messageRecorder
	current := newTestSession(t, upstream.write, nil)
	current.provider = "glm"
	current.routes = testModelCatalog(t)
	current.selections = &fakeProviderSelections{values: map[string]string{}}
	current.coordinator = &fakeHandoffCoordinator{}

	request := []byte(`{"id":125,"method":"thread/settings/update","params":{"threadId":"thr-a","model":"gpt-5.6-sol","collaborationMode":{"mode":"default","settings":{"model":"gpt-5.6-sol"}}}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	message, err := parseRPCMessage(upstream.messages()[0])
	if err != nil {
		t.Fatal(err)
	}
	assertRawString(t, message.params, "model", "glm-5.2")
	var collaboration struct {
		Settings struct {
			Model string `json:"model"`
		} `json:"settings"`
	}
	if json.Unmarshal(message.params["collaborationMode"], &collaboration) != nil ||
		collaboration.Settings.Model != "glm-5.2" {
		t.Fatalf("direct override collaborationMode = %s", message.params["collaborationMode"])
	}
}

func TestSessionMappedProviderSwitchVerifiesModelBeforeSaving(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		responseModel string
		wantSaved     bool
		wantFeedback  string
	}{
		{name: "matching model", responseModel: "glm-5.2", wantSaved: true, wantFeedback: "Provider switched to glm using model glm-5.2."},
		{name: "wrong model", responseModel: "gpt-5.6-sol"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			coordinator := &fakeHandoffCoordinator{}
			selections := &fakeProviderSelections{values: map[string]string{}}
			var current *session
			var upstream, downstream messageRecorder
			writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
				if err := upstream.write(ctx, messageType, payload); err != nil {
					return err
				}
				message, err := parseRPCMessage(payload)
				if err != nil {
					return err
				}
				if message.method == "thread/resume" {
					assertRawString(t, message.params, "modelProvider", "glm")
					assertRawString(t, message.params, "model", "glm-5.2")
					return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
						`{"id":%s,"result":{"thread":{"id":"thr-a"},"modelProvider":"glm","model":%q}}`, message.idKey, tt.responseModel)))
				}
				return nil
			}
			current = newTestSession(t, writer, downstream.write)
			current.provider = ""
			current.routes = testModelCatalog(t)
			current.selections = selections
			current.coordinator = coordinator
			current.stateMu.Lock()
			current.effective["thr-a"] = "glm"
			current.effectiveModel["thr-a"] = "gpt-5.6-sol"
			current.stateMu.Unlock()

			request := []byte(`{"id":123,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"/provider switch glm"}]}}`)
			if err := current.handleDownstreamText(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			_, saved := selections.values["thr-a"]
			if saved != tt.wantSaved {
				t.Fatalf("selection saved = %v, want %v", saved, tt.wantSaved)
			}
			if tt.wantSaved {
				if got := syntheticAgentFeedback(t, downstream.messages()); got != tt.wantFeedback {
					t.Fatalf("switch feedback = %q, want %q", got, tt.wantFeedback)
				}
				if coordinator.resubscribeRoute != (modelroute.Route{Provider: "glm", Model: "glm-5.2"}) {
					t.Fatalf("resubscribe route = %#v", coordinator.resubscribeRoute)
				}
			} else {
				messages := downstream.messages()
				if len(messages) != 1 || bytes.Contains(messages[0], []byte("glm-5.2")) {
					t.Fatalf("wrong-model response = %q", messages)
				}
			}
		})
	}
}

func TestSessionProviderStatusFailsClosedWhenSelectionCannotBeRead(t *testing.T) {
	t.Parallel()
	var upstream, downstream messageRecorder
	current := newTestSession(t, upstream.write, downstream.write)
	current.provider = ""
	current.coordinator = &fakeHandoffCoordinator{}
	current.selections = &fakeProviderSelections{getErr: errors.New("secret state path")}
	current.stateMu.Lock()
	current.effective["thr-a"] = "openai"
	current.stateMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := []byte(`{"id":20,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"/provider status"}]}}`)
	if err := current.handleDownstreamText(ctx, request); err != nil {
		t.Fatal(err)
	}
	if got := upstream.messages(); len(got) != 0 {
		t.Fatalf("failed status wrote to app-server: %q", got)
	}
	messages := downstream.messages()
	if len(messages) != 1 || bytes.Contains(messages[0], []byte("secret")) || bytes.Contains(messages[0], []byte("Runtime provider")) {
		t.Fatalf("failed status response = %q", messages)
	}
}

func TestSessionProviderCommandSwitchesWithoutForwardingModelTurn(t *testing.T) {
	t.Parallel()
	coordinator := &fakeHandoffCoordinator{}
	selections := &fakeProviderSelections{values: map[string]string{}}
	var current *session
	var upstream, downstream messageRecorder
	writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
		if err := upstream.write(ctx, messageType, payload); err != nil {
			return err
		}
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		if message.method == "thread/resume" {
			var requestedProvider string
			_ = json.Unmarshal(message.params["modelProvider"], &requestedProvider)
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"thread":{"id":"thr-a"},"modelProvider":%q}}`, message.idKey, requestedProvider)))
		}
		return nil
	}
	current = newTestSession(t, writer, downstream.write)
	current.provider = "sub2api"
	current.selections = selections
	current.coordinator = coordinator
	current.stateMu.Lock()
	current.effective["thr-a"] = "sub2api"
	current.stateMu.Unlock()

	request := []byte(`{"id":21,"method":"turn/start","params":{"threadId":"thr-a","input":[` +
		`{"type":"text","text":"$provider switch openai"},` +
		`{"type":"skill","name":"provider","path":"/home/user/.agents/skills/provider/SKILL.md"}]}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}

	upstreamMessages := upstream.messages()
	if len(upstreamMessages) != 1 {
		t.Fatalf("upstream message count = %d, want one internal resume", len(upstreamMessages))
	}
	resume, err := parseRPCMessage(upstreamMessages[0])
	if err != nil {
		t.Fatal(err)
	}
	var requestedProvider string
	_ = json.Unmarshal(resume.params["modelProvider"], &requestedProvider)
	if resume.method != "thread/resume" || requestedProvider != "openai" {
		t.Fatalf("internal resume = %#v", resume)
	}
	if got := selections.values["thr-a"]; got != "openai" {
		t.Fatalf("persisted provider = %q", got)
	}
	if current.isActive("thr-a") {
		t.Fatal("synthetic command left thread active")
	}
	if got := downstream.messages(); len(got) != 8 {
		t.Fatalf("synthetic downstream count = %d, want 8", len(got))
	}
	if got := syntheticUserCommand(t, downstream.messages()); got != "/provider switch openai" {
		t.Fatalf("synthetic provider command = %q", got)
	}
	if got := syntheticAgentFeedback(t, downstream.messages()); got != "Provider switched to openai." {
		t.Fatalf("synthetic provider feedback = %q", got)
	}
	if coordinator.prepareCalls != 1 || coordinator.prepareHandoffCalls != 1 ||
		coordinator.unsubscribeCalls != 1 || coordinator.resubscribeCalls != 1 ||
		coordinator.clearDirtyCalls != 1 {
		t.Fatalf("coordinator calls = %#v", coordinator)
	}
}

func TestSessionProviderCommandRejectsMalformedInputWithoutForwarding(t *testing.T) {
	t.Parallel()
	requests := []string{
		`{"id":22,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"/provider switch secret/bad"}]}}`,
		`{"id":22,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"[$provider](/home/user/.agents/skills/provider/SKILL.md) status trailing secret"}]}}`,
		`{"id":22,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"[$provider](/home/user/.agents/skills/provider/SKILL.md)status"}]}}`,
		`{"id":22,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"[$provider](/home/user/.agents/skills/provider/SKILL.md)switch openai"}]}}`,
	}
	for _, request := range requests {
		var upstream, downstream messageRecorder
		current := newTestSession(t, upstream.write, downstream.write)
		current.coordinator = &fakeHandoffCoordinator{}
		current.selections = &fakeProviderSelections{values: map[string]string{}}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := current.handleDownstreamText(ctx, []byte(request)); err != nil {
			t.Fatal(err)
		}
		if len(upstream.messages()) != 0 {
			t.Fatalf("malformed command reached upstream: %q", upstream.messages())
		}
		messages := downstream.messages()
		if len(messages) != 1 || bytes.Contains(messages[0], []byte("secret")) {
			t.Fatalf("malformed command response = %q", messages)
		}
		var response struct {
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(messages[0], &response) != nil || response.Error.Code != providerCommandErrorCode || response.Error.Message != invalidProviderCommandMessage {
			t.Fatalf("malformed command error = %#v", response)
		}
	}
}

func TestSessionProviderCommandDoesNotFakeSuccessWhenPersistenceFails(t *testing.T) {
	t.Parallel()
	coordinator := &fakeHandoffCoordinator{}
	selections := &fakeProviderSelections{values: map[string]string{}, setErr: errors.New("secret disk path")}
	var current *session
	var upstream, downstream messageRecorder
	writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
		if err := upstream.write(ctx, messageType, payload); err != nil {
			return err
		}
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		if message.method == "thread/resume" {
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"thread":{"id":"thr-a"},"modelProvider":"openai"}}`, message.idKey)))
		}
		return nil
	}
	current = newTestSession(t, writer, downstream.write)
	current.selections = selections
	current.coordinator = coordinator
	current.stateMu.Lock()
	current.effective["thr-a"] = "sub2api"
	current.stateMu.Unlock()

	request := []byte(`{"id":23,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"/provider switch openai"}]}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(upstream.messages()) != 1 {
		t.Fatalf("upstream messages = %q", upstream.messages())
	}
	messages := downstream.messages()
	if len(messages) != 1 || bytes.Contains(messages[0], []byte("secret")) || bytes.Contains(messages[0], []byte("Provider switched")) {
		t.Fatalf("persistence failure response = %q", messages)
	}
}

func TestSessionUsesStoredProviderForNormalTurn(t *testing.T) {
	t.Parallel()
	coordinator := &fakeHandoffCoordinator{}
	selections := &fakeProviderSelections{values: map[string]string{"thr-a": "sub2api"}}
	var current *session
	var upstream messageRecorder
	writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
		if err := upstream.write(ctx, messageType, payload); err != nil {
			return err
		}
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		if message.method == "thread/resume" {
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"thread":{"id":"thr-a"},"modelProvider":"sub2api"}}`, message.idKey)))
		}
		return nil
	}
	current = newTestSession(t, writer, nil)
	current.provider = "openai"
	current.selections = selections
	current.coordinator = coordinator
	current.stateMu.Lock()
	current.effective["thr-a"] = "openai"
	current.stateMu.Unlock()

	request := []byte(`{"id":24,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"ordinary prompt"}]}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	messages := upstream.messages()
	if len(messages) != 2 {
		t.Fatalf("upstream messages = %q", messages)
	}
	resume, _ := parseRPCMessage(messages[0])
	var requestedProvider string
	_ = json.Unmarshal(resume.params["modelProvider"], &requestedProvider)
	turn, _ := parseRPCMessage(messages[1])
	if requestedProvider != "sub2api" || turn.method != "turn/start" || turn.idKey != "24" {
		t.Fatalf("resume provider = %q, turn = %#v", requestedProvider, turn)
	}
}

func TestSessionResumeUsesStoredProvider(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dateDir := filepath.Join(home, "sessions", "2026", "08", "20")
	if err := os.MkdirAll(dateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rolloutPath := filepath.Join(dateDir, "rollout-2026-08-20T00-00-00-thr-a.jsonl")
	contents := `{"type":"session_meta","payload":{"id":"thr-a"}}` + "\n" + `{"type":"response_item","payload":{"type":"reasoning","id":"item_resume_stale"}}` + "\n"
	if err := os.WriteFile(rolloutPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	selections := &fakeProviderSelections{values: map[string]string{"thr-a": "sub2api"}}
	var current *session
	var upstream messageRecorder
	writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
		if err := upstream.write(ctx, messageType, payload); err != nil {
			return err
		}
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		if message.method == "thread/list" {
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"data":[{"id":"thr-a","path":%q,"modelProvider":"openai"}],"nextCursor":null}}`, message.idKey, rolloutPath)))
		}
		if message.method == "thread/read" {
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"thread":{"id":"thr-a","path":%q}}}`, message.idKey, rolloutPath)))
		}
		return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
			`{"id":%s,"result":{"thread":{"id":"thr-a"},"modelProvider":"sub2api"}}`, message.idKey)))
	}
	current = newTestSession(t, writer, nil)
	current.provider = "openai"
	current.codexHome = home
	current.selections = selections
	current.coordinator = &fakeHandoffCoordinator{}

	request := []byte(`{"id":25,"method":"thread/resume","params":{"threadId":"thr-a","path":"/stale/rollout.jsonl","modelProvider":"openai"}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	cleaned, err := os.ReadFile(rolloutPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cleaned), "item_resume_stale") {
		t.Fatalf("thread/resume forwarded before sanitation: %s", cleaned)
	}
	messages := upstream.messages()
	if len(messages) != 2 {
		t.Fatalf("upstream messages = %q", messages)
	}
	resume, err := parseRPCMessage(messages[1])
	if err != nil {
		t.Fatal(err)
	}
	var requestedProvider string
	_ = json.Unmarshal(resume.params["modelProvider"], &requestedProvider)
	if requestedProvider != "sub2api" {
		t.Fatalf("resume provider = %q", requestedProvider)
	}
	var requestedPath string
	_ = json.Unmarshal(resume.params["path"], &requestedPath)
	if requestedPath != rolloutPath {
		t.Fatalf("resume path = %q, want %q", requestedPath, rolloutPath)
	}
}

func TestSessionResumeSanitizesIdleLockedRolloutThroughHandoff(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dateDir := filepath.Join(home, "sessions", "2026", "08", "20")
	lockDir := filepath.Join(home, "thread-writer-locks")
	if err := os.MkdirAll(dateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rolloutPath := filepath.Join(dateDir, "rollout-2026-08-20T00-00-00-thr-a.jsonl")
	contents := `{"type":"session_meta","payload":{"id":"thr-a"}}` + "\n" + `{"type":"response_item","payload":{"type":"reasoning","id":"item_resume_stale"}}` + "\n"
	if err := os.WriteFile(rolloutPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(lockDir, "thr-a.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	selections := &fakeProviderSelections{values: map[string]string{"thr-a": "sub2api"}}
	coordinator := &fakeHandoffCoordinator{unsubscribeHook: func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	}}
	var current *session
	var upstream messageRecorder
	writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
		if err := upstream.write(ctx, messageType, payload); err != nil {
			return err
		}
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		if message.method == "thread/list" {
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"data":[{"id":"thr-a","path":%q,"modelProvider":"openai"}],"nextCursor":null}}`, message.idKey, rolloutPath)))
		}
		if message.method == "thread/read" {
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"thread":{"id":"thr-a","path":%q}}}`, message.idKey, rolloutPath)))
		}
		return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
			`{"id":%s,"result":{"thread":{"id":"thr-a"},"modelProvider":"sub2api"}}`, message.idKey)))
	}
	current = newTestSession(t, writer, nil)
	current.provider = "openai"
	current.codexHome = home
	current.selections = selections
	current.coordinator = coordinator

	request := []byte(`{"id":26,"method":"thread/resume","params":{"threadId":"thr-a","path":"/stale/rollout.jsonl","modelProvider":"openai"}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	cleaned, err := os.ReadFile(rolloutPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cleaned), "item_resume_stale") {
		t.Fatalf("thread/resume forwarded before sanitation: %s", cleaned)
	}
	if coordinator.unsubscribeCalls != 1 || coordinator.resubscribeCalls != 1 {
		t.Fatalf("handoff calls = %#v, want one unsubscribe and resubscribe", coordinator)
	}
	messages := upstream.messages()
	var sawDesktopResume bool
	for _, payload := range messages {
		message, parseErr := parseRPCMessage(payload)
		if parseErr == nil && message.method == "thread/resume" && message.idKey == "26" {
			sawDesktopResume = true
		}
	}
	if !sawDesktopResume {
		t.Fatalf("desktop resume was not forwarded after sanitation: %q", messages)
	}
}

func TestSessionResumeSameProviderSkipsLockedRolloutSanitation(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dateDir := filepath.Join(home, "sessions", "2026", "08", "23")
	lockDir := filepath.Join(home, "thread-writer-locks")
	if err := os.MkdirAll(dateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rolloutPath := filepath.Join(dateDir, "rollout-2026-08-23T00-00-00-thr-a.jsonl")
	contents := `{"type":"session_meta","payload":{"id":"thr-a"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"reasoning","id":"item_same_provider"}}` + "\n"
	if err := os.WriteFile(rolloutPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(lockDir, "thr-a.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	selections := &fakeProviderSelections{values: map[string]string{"thr-a": "sub2api"}}
	coordinator := &fakeHandoffCoordinator{}
	var current *session
	var upstream messageRecorder
	writer := func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
		if err := upstream.write(ctx, messageType, payload); err != nil {
			return err
		}
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
		}
		switch message.method {
		case "thread/list":
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"data":[{"id":"thr-a","path":%q,"modelProvider":"sub2api"}],"nextCursor":null}}`,
				message.idKey, rolloutPath)))
		case "thread/resume":
			return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
				`{"id":%s,"result":{"thread":{"id":"thr-a"},"modelProvider":"sub2api"}}`, message.idKey)))
		default:
			return nil
		}
	}
	current = newTestSession(t, writer, nil)
	current.codexHome = home
	current.selections = selections
	current.coordinator = coordinator

	request := []byte(`{"id":27,"method":"thread/resume","params":{"threadId":"thr-a"}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if coordinator.unsubscribeCalls != 0 || coordinator.resubscribeCalls != 0 {
		t.Fatalf("same-provider resume triggered handoff: %#v", coordinator)
	}
	messages := upstream.messages()
	var desktopResume bool
	for _, payload := range messages {
		message, parseErr := parseRPCMessage(payload)
		if parseErr == nil && message.method == "thread/resume" && message.idKey == "27" {
			desktopResume = true
		}
	}
	if !desktopResume {
		t.Fatalf("same-provider resume was not forwarded: %q", messages)
	}
	unchanged, err := os.ReadFile(rolloutPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unchanged), "item_same_provider") {
		t.Fatalf("same-provider rollout was unexpectedly rewritten: %s", unchanged)
	}
}

func TestSessionStoredProviderReadFailureFailsClosed(t *testing.T) {
	t.Parallel()
	var upstream, downstream messageRecorder
	current := newTestSession(t, upstream.write, downstream.write)
	current.provider = "openai"
	current.selections = &fakeProviderSelections{getErr: errors.New("secret state")}
	current.coordinator = &fakeHandoffCoordinator{}

	request := []byte(`{"id":26,"method":"turn/start","params":{"threadId":"thr-a","input":[]}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(upstream.messages()) != 0 {
		t.Fatalf("state read failure reached upstream: %q", upstream.messages())
	}
	messages := downstream.messages()
	if len(messages) != 1 || bytes.Contains(messages[0], []byte("secret")) {
		t.Fatalf("state read failure response = %q", messages)
	}
}

func TestSessionSuppressesOnlyMatchingRecoveryLifecycle(t *testing.T) {
	t.Parallel()
	var downstream messageRecorder
	current := newTestSession(t, nil, downstream.write)
	if status := current.BeginRecovery("root", []string{"child", "root"}); status != handoff.StatusRecoveryReady {
		t.Fatalf("BeginRecovery() status = %q", status)
	}
	messages := [][]byte{
		[]byte(`{"method":"thread/archived","params":{"threadId":"root"}}`),
		[]byte(`{"method":"thread/status/changed","params":{"threadId":"child","status":{"type":"idle"}}}`),
		[]byte(`{"method":"thread/archived","params":{"threadId":"other"}}`),
		[]byte(`{"method":"turn/started","params":{"threadId":"root","turn":{"id":"turn-a"}}}`),
	}
	for _, message := range messages {
		if err := current.handleUpstreamText(context.Background(), message); err != nil {
			t.Fatal(err)
		}
	}
	if got := downstream.messages(); len(got) != 2 || !bytes.Equal(got[0], messages[2]) || !bytes.Equal(got[1], messages[3]) {
		t.Fatalf("visible messages = %q", got)
	}
	if status := current.EndRecovery("root"); status != handoff.StatusRecoveryEnded {
		t.Fatalf("EndRecovery() status = %q", status)
	}
	after := []byte(`{"method":"thread/unarchived","params":{"threadId":"root"}}`)
	if err := current.handleUpstreamText(context.Background(), after); err != nil {
		t.Fatal(err)
	}
	got := downstream.messages()
	if len(got) != 3 || !bytes.Equal(got[2], after) {
		t.Fatalf("messages after end = %q", got)
	}
}

func TestSessionV2JournalRepairRejectsLegacyPeerBeforeRecoveryMutation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := newHandoffAppServer(t, ctx, true)
	server.mu.Lock()
	server.archived["thr-shared"] = true
	server.mu.Unlock()
	store, err := recovery.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := recovery.Journal{
		Version: 2, RootID: "thr-shared", Provider: "glm", Model: "glm-5.2", Phase: "restoring",
		Threads: []string{"thr-shared"}, Remaining: []string{"thr-shared"},
	}
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	coordinator := &fakeHandoffCoordinator{prepareHandoffErr: errors.New("legacy peer")}
	current := newTestSession(t, nil, nil)
	current.appServerSocket = server.socket
	current.coordinator = coordinator
	current.recoveries = store

	if err := current.repairRecoveryJournal(ctx, "thr-shared"); err == nil {
		t.Fatal("repairRecoveryJournal() error = nil with legacy peer")
	}
	if coordinator.prepareCalls != 1 || coordinator.prepareHandoffCalls != 1 || coordinator.beginRecoveryCalls != 0 {
		t.Fatalf("coordinator calls = %#v", coordinator)
	}
	server.mu.Lock()
	connections := server.nextClient
	unarchived := append([]string(nil), server.unarchived...)
	resumes := append([]handoffResumeRecord(nil), server.resumes...)
	server.mu.Unlock()
	if connections != 0 || len(unarchived) != 0 || len(resumes) != 0 {
		t.Fatalf("app-server mutations: connections=%d unarchived=%v resumes=%v", connections, unarchived, resumes)
	}
	got, found, err := store.Load("thr-shared")
	if err != nil || !found || !reflect.DeepEqual(got, want) {
		t.Fatalf("journal after rejected repair = %#v, %v, %v; want unchanged %#v", got, found, err, want)
	}
}

type messageRecorder struct {
	mu       sync.Mutex
	payloads [][]byte
}

type fakeProviderSelections struct {
	values   map[string]string
	models   map[string]string
	getErr   error
	setErr   error
	setCalls int
}

func (selections *fakeProviderSelections) GetRoute(threadID string) (selection.Value, bool, error) {
	provider, ok, err := selections.Get(threadID)
	if err != nil || !ok {
		return selection.Value{}, ok, err
	}
	return selection.Value{Provider: provider, Model: selections.models[threadID]}, true, nil
}

func (selections *fakeProviderSelections) SetRoute(threadID string, value selection.Value) error {
	if err := selections.Set(threadID, value.Provider); err != nil {
		return err
	}
	if selections.models == nil {
		selections.models = make(map[string]string)
	}
	selections.models[threadID] = value.Model
	return nil
}

func (selections *fakeProviderSelections) Get(threadID string) (string, bool, error) {
	if selections.getErr != nil {
		return "", false, selections.getErr
	}
	value, ok := selections.values[threadID]
	return value, ok, nil
}

func (selections *fakeProviderSelections) Set(threadID, provider string) error {
	selections.setCalls++
	if selections.setErr != nil {
		return selections.setErr
	}
	if selections.values == nil {
		selections.values = make(map[string]string)
	}
	selections.values[threadID] = provider
	return nil
}

func syntheticAgentFeedback(t *testing.T, messages [][]byte) string {
	t.Helper()
	for _, payload := range messages {
		message, err := parseRPCMessage(payload)
		if err != nil || message.method != "item/agentMessage/delta" {
			continue
		}
		var feedback string
		if json.Unmarshal(message.params["delta"], &feedback) != nil {
			t.Fatalf("invalid synthetic feedback: %s", message.params["delta"])
		}
		return feedback
	}
	return ""
}

func syntheticUserCommand(t *testing.T, messages [][]byte) string {
	t.Helper()
	for _, payload := range messages {
		message, err := parseRPCMessage(payload)
		if err != nil || message.method != "item/started" {
			continue
		}
		var item struct {
			Type    string `json:"type"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(message.params["item"], &item) != nil {
			continue
		}
		if item.Type == "userMessage" && len(item.Content) == 1 {
			return item.Content[0].Text
		}
	}
	return ""
}

func (recorder *messageRecorder) write(_ context.Context, messageType websocket.MessageType, payload []byte) error {
	if messageType != websocket.MessageText {
		return nil
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.payloads = append(recorder.payloads, append([]byte(nil), payload...))
	return nil
}

func (recorder *messageRecorder) messages() [][]byte {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	result := make([][]byte, len(recorder.payloads))
	for index := range recorder.payloads {
		result[index] = append([]byte(nil), recorder.payloads[index]...)
	}
	return result
}

func newTestSession(t *testing.T, upstreamWrite, downstreamWrite websocketWriteFunc) *session {
	t.Helper()
	if upstreamWrite == nil {
		upstreamWrite = func(context.Context, websocket.MessageType, []byte) error { return nil }
	}
	if downstreamWrite == nil {
		downstreamWrite = func(context.Context, websocket.MessageType, []byte) error { return nil }
	}
	session, err := newSessionState("sub2api", nil, "/tmp/app-server.sock", upstreamWrite, downstreamWrite)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.closeState)
	return session
}

type fakeHandoffCoordinator struct {
	lockCalls                int
	prepareCalls             int
	prepareHandoffCalls      int
	unsubscribeCalls         int
	resubscribeCalls         int
	restoreCalls             int
	markDirtyCalls           int
	clearDirtyCalls          int
	dirtyStageCalls          []handoff.DirtyStage
	releaseCalls             int
	beginRecoveryCalls       int
	prepareErr               error
	prepareAfterReconcileErr error
	prepareHandoffErr        error
	unsubscribeErr           error
	unsubscribeHook          func()
	resubscribeErr           error
	restoreErr               error
	dirty                    bool
	dirtyStage               handoff.DirtyStage
	resubscribeRoute         modelroute.Route
	reconcileCalls           int
	reconcileErr             error
}

func (coordinator *fakeHandoffCoordinator) BeginRecoveryAll(context.Context, string, []string) error {
	coordinator.beginRecoveryCalls++
	return nil
}

func (coordinator *fakeHandoffCoordinator) EndRecoveryAll(context.Context, string) error {
	return nil
}

func (coordinator *fakeHandoffCoordinator) LockThread(context.Context, string) (func(), error) {
	coordinator.lockCalls++
	return func() { coordinator.releaseCalls++ }, nil
}

func (coordinator *fakeHandoffCoordinator) PrepareAll(context.Context, string) error {
	coordinator.prepareCalls++
	if coordinator.reconcileCalls > 0 {
		return coordinator.prepareAfterReconcileErr
	}
	return coordinator.prepareErr
}

func (coordinator *fakeHandoffCoordinator) ReconcileIdleAll(context.Context, string) error {
	coordinator.reconcileCalls++
	return coordinator.reconcileErr
}

func (coordinator *fakeHandoffCoordinator) PrepareHandoffAll(context.Context, string) error {
	coordinator.prepareHandoffCalls++
	return coordinator.prepareHandoffErr
}

func (coordinator *fakeHandoffCoordinator) PrepareRecoveryAll(context.Context, string) error {
	return nil
}

func (coordinator *fakeHandoffCoordinator) UnsubscribeAll(context.Context, string) error {
	coordinator.unsubscribeCalls++
	if coordinator.unsubscribeHook != nil {
		coordinator.unsubscribeHook()
	}
	return coordinator.unsubscribeErr
}

func (coordinator *fakeHandoffCoordinator) ResubscribeAll(_ context.Context, _ string, route modelroute.Route) error {
	coordinator.resubscribeCalls++
	coordinator.resubscribeRoute = route
	return coordinator.resubscribeErr
}

func (coordinator *fakeHandoffCoordinator) RestoreAll(context.Context, string) error {
	coordinator.restoreCalls++
	return coordinator.restoreErr
}

func (coordinator *fakeHandoffCoordinator) MarkDirty(string) error {
	coordinator.markDirtyCalls++
	coordinator.dirty = true
	coordinator.dirtyStage = handoff.DirtyStagePrepared
	coordinator.dirtyStageCalls = append(coordinator.dirtyStageCalls, handoff.DirtyStagePrepared)
	return nil
}

func (coordinator *fakeHandoffCoordinator) SetDirtyStage(_ string, stage handoff.DirtyStage) error {
	coordinator.dirtyStage = stage
	coordinator.dirtyStageCalls = append(coordinator.dirtyStageCalls, stage)
	return nil
}

func (coordinator *fakeHandoffCoordinator) IsDirty(string) bool {
	return coordinator.dirty
}

func (coordinator *fakeHandoffCoordinator) ReadDirtyStage(string) (handoff.DirtyStage, bool, error) {
	return coordinator.dirtyStage, coordinator.dirty, nil
}

func (coordinator *fakeHandoffCoordinator) ClearDirty(string) error {
	coordinator.clearDirtyCalls++
	coordinator.dirty = false
	coordinator.dirtyStage = ""
	return nil
}

func (coordinator *fakeHandoffCoordinator) Close() error { return nil }

func stringsHasPrefix(value, prefix string) bool {
	return len(value) >= len(prefix) && value[:len(prefix)] == prefix
}

func testModelCatalog(t *testing.T) *modelroute.Catalog {
	t.Helper()
	directory := t.TempDir()
	data := []byte(`{"openai":"gpt-5.6-sol","sub2api":"gpt-5.6-sol","glm":"glm-5.2"}`)
	if err := os.WriteFile(filepath.Join(directory, "models.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := modelroute.Load(directory)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func testMultiModelCatalog(t *testing.T) *modelroute.Catalog {
	t.Helper()
	directory := t.TempDir()
	data := []byte(`{"glm":{"default":"glm-5.3","models":["glm-5.3","glm-5.2"]}}`)
	if err := os.WriteFile(filepath.Join(directory, "models.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := modelroute.Load(directory)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func assertRawString(t *testing.T, values map[string]json.RawMessage, key, want string) {
	t.Helper()
	var got string
	if json.Unmarshal(values[key], &got) != nil || got != want {
		t.Fatalf("%s = %q, want %q", key, got, want)
	}
}
