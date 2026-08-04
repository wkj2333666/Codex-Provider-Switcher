package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/handoff"
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
	status, err := current.Resubscribe(context.Background(), "thr-a", "sub2api")
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
	status, err := current.Resubscribe(context.Background(), "thr-a", "sub2api")
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
		coordinator.resubscribeCalls != 1 || coordinator.releaseCalls != 1 {
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

func TestSessionReturnsStaticErrorWhenHandoffFails(t *testing.T) {
	t.Parallel()
	coordinator := &fakeHandoffCoordinator{prepareErr: errors.New("secret peer path")}
	var upstream, downstream messageRecorder
	current := newTestSession(t, upstream.write, downstream.write)
	current.coordinator = coordinator
	current.stateMu.Lock()
	current.effective["thr-a"] = "openai"
	current.stateMu.Unlock()

	request := []byte(`{"id":"desktop-secret-id","method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"secret prompt"}]}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(upstream.messages()) != 0 {
		t.Fatalf("failed handoff wrote upstream: %q", upstream.messages())
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

func newResponsiveResumeSession(t *testing.T, coordinator handoffCoordinator, provider string) *session {
	t.Helper()
	var current *session
	writer := func(ctx context.Context, _ websocket.MessageType, payload []byte) error {
		message, err := parseRPCMessage(payload)
		if err != nil {
			return err
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
		return current.handleUpstreamText(ctx, []byte(fmt.Sprintf(
			`{"id":%s,"result":{"thread":{"id":"thr-a"},"modelProvider":"sub2api"}}`, message.idKey)))
	}
	current = newTestSession(t, writer, nil)
	current.provider = "openai"
	current.selections = selections
	current.coordinator = &fakeHandoffCoordinator{}

	request := []byte(`{"id":25,"method":"thread/resume","params":{"threadId":"thr-a","modelProvider":"openai"}}`)
	if err := current.handleDownstreamText(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	messages := upstream.messages()
	if len(messages) != 1 {
		t.Fatalf("upstream messages = %q", messages)
	}
	resume, err := parseRPCMessage(messages[0])
	if err != nil {
		t.Fatal(err)
	}
	var requestedProvider string
	_ = json.Unmarshal(resume.params["modelProvider"], &requestedProvider)
	if requestedProvider != "sub2api" {
		t.Fatalf("resume provider = %q", requestedProvider)
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

type messageRecorder struct {
	mu       sync.Mutex
	payloads [][]byte
}

type fakeProviderSelections struct {
	values   map[string]string
	getErr   error
	setErr   error
	setCalls int
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
	lockCalls           int
	prepareCalls        int
	prepareHandoffCalls int
	unsubscribeCalls    int
	resubscribeCalls    int
	restoreCalls        int
	markDirtyCalls      int
	clearDirtyCalls     int
	dirtyStageCalls     []handoff.DirtyStage
	releaseCalls        int
	prepareErr          error
	prepareHandoffErr   error
	unsubscribeErr      error
	resubscribeErr      error
	restoreErr          error
	dirty               bool
}

func (coordinator *fakeHandoffCoordinator) BeginRecoveryAll(context.Context, string, []string) error {
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
	return coordinator.prepareErr
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
	return coordinator.unsubscribeErr
}

func (coordinator *fakeHandoffCoordinator) ResubscribeAll(context.Context, string, string) error {
	coordinator.resubscribeCalls++
	return coordinator.resubscribeErr
}

func (coordinator *fakeHandoffCoordinator) RestoreAll(context.Context, string) error {
	coordinator.restoreCalls++
	return coordinator.restoreErr
}

func (coordinator *fakeHandoffCoordinator) MarkDirty(string) error {
	coordinator.markDirtyCalls++
	coordinator.dirty = true
	coordinator.dirtyStageCalls = append(coordinator.dirtyStageCalls, handoff.DirtyStagePrepared)
	return nil
}

func (coordinator *fakeHandoffCoordinator) SetDirtyStage(_ string, stage handoff.DirtyStage) error {
	coordinator.dirtyStageCalls = append(coordinator.dirtyStageCalls, stage)
	return nil
}

func (coordinator *fakeHandoffCoordinator) IsDirty(string) bool {
	return coordinator.dirty
}

func (coordinator *fakeHandoffCoordinator) ClearDirty(string) error {
	coordinator.clearDirtyCalls++
	coordinator.dirty = false
	return nil
}

func (coordinator *fakeHandoffCoordinator) Close() error { return nil }

func stringsHasPrefix(value, prefix string) bool {
	return len(value) >= len(prefix) && value[:len(prefix)] == prefix
}
