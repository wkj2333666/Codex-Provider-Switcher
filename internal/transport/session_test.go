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

type messageRecorder struct {
	mu       sync.Mutex
	payloads [][]byte
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
	session, err := newSessionState("sub2api", "/tmp/app-server.sock", upstreamWrite, downstreamWrite)
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
	releaseCalls        int
	prepareErr          error
	prepareHandoffErr   error
	unsubscribeErr      error
	resubscribeErr      error
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

func (coordinator *fakeHandoffCoordinator) UnsubscribeAll(context.Context, string) error {
	coordinator.unsubscribeCalls++
	return coordinator.unsubscribeErr
}

func (coordinator *fakeHandoffCoordinator) ResubscribeAll(context.Context, string, string) error {
	coordinator.resubscribeCalls++
	return coordinator.resubscribeErr
}

func (coordinator *fakeHandoffCoordinator) Close() error { return nil }

func stringsHasPrefix(value, prefix string) bool {
	return len(value) >= len(prefix) && value[:len(prefix)] == prefix
}
