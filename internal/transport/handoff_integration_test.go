package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/config"
)

func TestRunSwitchesProviderOnNextTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server := newHandoffAppServer(t, ctx, true)
	openai, openaiDone := dialProviderProxy(t, ctx, server.socket, "openai")
	sub2api, sub2apiDone := dialProviderProxy(t, ctx, server.socket, "sub2api")
	defer openai.CloseNow()
	defer sub2api.CloseNow()

	initializeTestClient(t, ctx, openai)
	initializeTestClient(t, ctx, sub2api)
	if provider := resumeTestThread(t, ctx, openai, 2); provider != "openai" {
		t.Fatalf("openai resume provider = %q", provider)
	}
	if provider := resumeTestThread(t, ctx, sub2api, 2); provider != "openai" {
		t.Fatalf("sub2api initial resume provider = %q, want loaded openai", provider)
	}

	subVisible := sendTestTurnAndCollect(t, ctx, sub2api, 3)
	if record := <-server.turns; record.threadID != "thr-shared" || record.provider != "sub2api" {
		t.Fatalf("sub2api turn = %#v", record)
	}
	assertNoInternalMessages(t, subVisible)
	if subscribers := server.subscriberCount(); subscribers != 2 {
		t.Fatalf("subscriber count after switch = %d, want 2", subscribers)
	}
	readUntilMethod(t, ctx, openai, "turn/started")
	readUntilMethod(t, ctx, openai, "turn/completed")

	openVisible := sendTestTurnAndCollect(t, ctx, openai, 3)
	if record := <-server.turns; record.threadID != "thr-shared" || record.provider != "openai" {
		t.Fatalf("openai turn = %#v", record)
	}
	assertNoInternalMessages(t, openVisible)

	cancel()
	_ = openai.CloseNow()
	_ = sub2api.CloseNow()
	waitProxyDone(t, openaiDone)
	waitProxyDone(t, sub2apiDone)
}

func TestRunRejectsConcurrentSameProviderTurnBeforePeerNotification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server := newHandoffAppServer(t, ctx, false)
	server.startedGate = make(chan struct{})
	first, firstDone := dialProviderProxy(t, ctx, server.socket, "openai")
	second, secondDone := dialProviderProxy(t, ctx, server.socket, "openai")
	defer first.CloseNow()
	defer second.CloseNow()

	initializeTestClient(t, ctx, first)
	initializeTestClient(t, ctx, second)
	resumeTestThread(t, ctx, first, 2)
	resumeTestThread(t, ctx, second, 2)

	sendRPC(t, ctx, first, 3, "turn/start", map[string]any{
		"threadId": "thr-shared",
		"input":    []any{},
	})
	if record := <-server.turns; record.provider != "openai" {
		t.Fatalf("first turn = %#v", record)
	}
	sendRPC(t, ctx, second, 3, "turn/start", map[string]any{
		"threadId": "thr-shared",
		"input":    []any{},
	})
	response := readResponse(t, ctx, second, `3`)
	if response.errorCode != handoffErrorCode || response.errorMessage != handoffUnavailableMessage {
		t.Fatalf("concurrent turn response = %#v", response)
	}
	if calls := server.turnStartCallCount(); calls != 1 {
		t.Fatalf("app-server turn/start calls = %d, want 1", calls)
	}

	cancel()
	_ = first.CloseNow()
	_ = second.CloseNow()
	waitProxyDone(t, firstDone)
	waitProxyDone(t, secondDone)
}

func TestRunRejectsHandoffWhilePeerTurnIsActive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server := newHandoffAppServer(t, ctx, false)
	openai, openaiDone := dialProviderProxy(t, ctx, server.socket, "openai")
	sub2api, sub2apiDone := dialProviderProxy(t, ctx, server.socket, "sub2api")
	defer openai.CloseNow()
	defer sub2api.CloseNow()

	initializeTestClient(t, ctx, openai)
	initializeTestClient(t, ctx, sub2api)
	resumeTestThread(t, ctx, openai, 2)
	resumeTestThread(t, ctx, sub2api, 2)

	sendRPC(t, ctx, openai, 3, "turn/start", map[string]any{
		"threadId": "thr-shared",
		"input":    []any{},
	})
	readUntilMethod(t, ctx, openai, "turn/started")
	if record := <-server.turns; record.provider != "openai" {
		t.Fatalf("active turn = %#v", record)
	}

	sendRPC(t, ctx, sub2api, 3, "turn/start", map[string]any{
		"threadId": "thr-shared",
		"input":    []any{},
	})
	response := readResponse(t, ctx, sub2api, `3`)
	if response.errorCode != handoffErrorCode || response.errorMessage != handoffUnavailableMessage {
		t.Fatalf("handoff response = %#v", response)
	}
	select {
	case unexpected := <-server.turns:
		t.Fatalf("blocked turn reached app-server: %#v", unexpected)
	case <-time.After(100 * time.Millisecond):
	}
	if subscribers := server.subscriberCount(); subscribers != 2 {
		t.Fatalf("subscriber count = %d, want 2 after prepare rejection", subscribers)
	}

	cancel()
	_ = openai.CloseNow()
	_ = sub2api.CloseNow()
	waitProxyDone(t, openaiDone)
	waitProxyDone(t, sub2apiDone)
}

func TestRunRejectsHandoffWithNonCooperatingSubscriber(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server := newHandoffAppServer(t, ctx, true)
	raw := dialRawAppServer(t, ctx, server.socket)
	defer raw.CloseNow()
	initializeTestClient(t, ctx, raw)
	if provider := resumeTestThread(t, ctx, raw, 2); provider != "openai" {
		t.Fatalf("raw resume provider = %q", provider)
	}

	sub2api, sub2apiDone := dialProviderProxy(t, ctx, server.socket, "sub2api")
	defer sub2api.CloseNow()
	initializeTestClient(t, ctx, sub2api)
	if provider := resumeTestThread(t, ctx, sub2api, 2); provider != "openai" {
		t.Fatalf("sub2api initial resume provider = %q", provider)
	}
	sendRPC(t, ctx, sub2api, 3, "turn/start", map[string]any{
		"threadId": "thr-shared",
		"input":    []any{},
	})
	response := readResponse(t, ctx, sub2api, `3`)
	if response.errorCode != handoffErrorCode {
		t.Fatalf("handoff response = %#v", response)
	}
	select {
	case unexpected := <-server.turns:
		t.Fatalf("blocked turn reached app-server: %#v", unexpected)
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	_ = raw.CloseNow()
	_ = sub2api.CloseNow()
	waitProxyDone(t, sub2apiDone)
}

type handoffAppServer struct {
	socket       string
	ctx          context.Context
	autoComplete bool

	mu          sync.Mutex
	writeMu     sync.Mutex
	nextClient  int
	provider    string
	active      bool
	subscribers map[int]*websocket.Conn
	startedGate chan struct{}
	turnStarts  int
	turns       chan handoffTurnRecord
}

type handoffTurnRecord struct {
	threadID string
	provider string
}

func newHandoffAppServer(t *testing.T, ctx context.Context, autoComplete bool) *handoffAppServer {
	t.Helper()
	server := &handoffAppServer{
		ctx:          ctx,
		autoComplete: autoComplete,
		provider:     "openai",
		subscribers:  make(map[int]*websocket.Conn),
		turns:        make(chan handoffTurnRecord, 8),
	}
	server.socket = startUnixHTTPServer(t, http.HandlerFunc(server.handleUpgrade))
	return server
}

func (server *handoffAppServer) handleUpgrade(writer http.ResponseWriter, request *http.Request) {
	connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	defer connection.CloseNow()
	server.mu.Lock()
	server.nextClient++
	clientID := server.nextClient
	server.mu.Unlock()
	defer func() {
		server.mu.Lock()
		delete(server.subscribers, clientID)
		server.mu.Unlock()
	}()

	for {
		messageType, payload, err := connection.Read(server.ctx)
		if err != nil {
			return
		}
		if messageType != websocket.MessageText {
			continue
		}
		message, err := parseRPCMessage(payload)
		if err != nil || message.kind != rpcRequest {
			continue
		}
		switch message.method {
		case "initialize":
			server.writeResult(connection, message.id, map[string]any{"userAgent": "handoff-test"})
		case "thread/resume":
			server.handleResume(connection, clientID, message)
		case "thread/unsubscribe":
			server.handleUnsubscribe(connection, clientID, message)
		case "turn/start":
			server.handleTurnStart(connection, message)
		default:
			server.writeResult(connection, message.id, map[string]any{})
		}
	}
}

func (server *handoffAppServer) handleResume(connection *websocket.Conn, clientID int, message rpcMessage) {
	requestedProvider := ""
	_ = json.Unmarshal(message.params["modelProvider"], &requestedProvider)
	server.mu.Lock()
	if requestedProvider != "" && requestedProvider != server.provider && len(server.subscribers) == 0 && !server.active {
		server.provider = requestedProvider
	}
	server.subscribers[clientID] = connection
	provider := server.provider
	server.mu.Unlock()
	server.writeResult(connection, message.id, map[string]any{
		"thread":        map[string]any{"id": "thr-shared", "turns": []any{}},
		"modelProvider": provider,
	})
}

func (server *handoffAppServer) handleUnsubscribe(connection *websocket.Conn, clientID int, message rpcMessage) {
	server.mu.Lock()
	_, subscribed := server.subscribers[clientID]
	delete(server.subscribers, clientID)
	server.mu.Unlock()
	status := "notSubscribed"
	if subscribed {
		status = "unsubscribed"
	}
	server.writeResult(connection, message.id, map[string]any{"status": status})
}

func (server *handoffAppServer) handleTurnStart(connection *websocket.Conn, message rpcMessage) {
	threadID, _ := requireThreadID(message)
	server.mu.Lock()
	server.turnStarts++
	if server.active {
		server.mu.Unlock()
		server.writeError(connection, message.id, -32001, "turn already active")
		return
	}
	server.active = true
	provider := server.provider
	subscribers := server.subscriberConnectionsLocked()
	startedGate := server.startedGate
	server.mu.Unlock()
	server.turns <- handoffTurnRecord{threadID: threadID, provider: provider}
	server.writeResult(connection, message.id, map[string]any{
		"turn": map[string]any{"id": "turn-test", "status": "inProgress", "items": []any{}},
	})
	if startedGate != nil {
		select {
		case <-startedGate:
		case <-server.ctx.Done():
			return
		}
	}
	server.broadcastNotification(subscribers, "turn/started", map[string]any{
		"threadId": threadID,
		"turn":     map[string]any{"id": "turn-test", "status": "inProgress", "items": []any{}},
	})
	if server.autoComplete {
		server.mu.Lock()
		server.active = false
		subscribers = server.subscriberConnectionsLocked()
		server.mu.Unlock()
		server.broadcastNotification(subscribers, "turn/completed", map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": "turn-test", "status": "completed", "items": []any{}},
		})
	}
}

func (server *handoffAppServer) subscriberCount() int {
	server.mu.Lock()
	defer server.mu.Unlock()
	return len(server.subscribers)
}

func (server *handoffAppServer) turnStartCallCount() int {
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.turnStarts
}

func (server *handoffAppServer) subscriberConnectionsLocked() []*websocket.Conn {
	connections := make([]*websocket.Conn, 0, len(server.subscribers))
	for _, connection := range server.subscribers {
		connections = append(connections, connection)
	}
	return connections
}

func (server *handoffAppServer) writeResult(connection *websocket.Conn, id json.RawMessage, result any) {
	server.writeJSON(connection, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (server *handoffAppServer) writeNotification(connection *websocket.Conn, method string, params any) {
	server.writeJSON(connection, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (server *handoffAppServer) broadcastNotification(connections []*websocket.Conn, method string, params any) {
	for _, connection := range connections {
		server.writeNotification(connection, method, params)
	}
}

func (server *handoffAppServer) writeError(connection *websocket.Conn, id json.RawMessage, code int, message string) {
	server.writeJSON(connection, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
}

func (server *handoffAppServer) writeJSON(connection *websocket.Conn, value any) {
	payload, err := json.Marshal(value)
	if err != nil {
		return
	}
	server.writeMu.Lock()
	defer server.writeMu.Unlock()
	_ = connection.Write(server.ctx, websocket.MessageText, payload)
}

type visibleRPC struct {
	id           string
	method       string
	errorCode    int
	errorMessage string
}

func dialProviderProxy(t *testing.T, ctx context.Context, socket, provider string) (*websocket.Conn, <-chan error) {
	t.Helper()
	clientStream, switcherStream := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Config: config.Config{Provider: provider, Socket: socket},
			Stdin:  switcherStream,
			Stdout: switcherStream,
		})
	}()
	client, response, err := websocket.Dial(ctx, "ws://desktop.test/", &websocket.DialOptions{
		HTTPClient: pipeHTTPClient(clientStream),
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d", response.StatusCode)
	}
	return client, done
}

func dialRawAppServer(t *testing.T, ctx context.Context, socket string) *websocket.Conn {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(dialContext context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(dialContext, "unix", socket)
		},
	}}
	connection, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

func initializeTestClient(t *testing.T, ctx context.Context, connection *websocket.Conn) {
	t.Helper()
	sendRPC(t, ctx, connection, 1, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "handoff-test", "version": "1"},
	})
	readResponse(t, ctx, connection, `1`)
	if err := connection.Write(ctx, websocket.MessageText, []byte(`{"method":"initialized"}`)); err != nil {
		t.Fatal(err)
	}
}

func resumeTestThread(t *testing.T, ctx context.Context, connection *websocket.Conn, id int) string {
	t.Helper()
	sendRPC(t, ctx, connection, id, "thread/resume", map[string]any{"threadId": "thr-shared"})
	response := readResponse(t, ctx, connection, fmt.Sprint(id))
	return responseProvider(t, response.raw)
}

func sendTestTurnAndCollect(t *testing.T, ctx context.Context, connection *websocket.Conn, id int) []visibleRPC {
	t.Helper()
	sendRPC(t, ctx, connection, id, "turn/start", map[string]any{
		"threadId": "thr-shared",
		"input":    []any{},
	})
	var visible []visibleRPC
	for {
		message := readVisibleRPC(t, ctx, connection)
		visible = append(visible, message)
		if message.method == "turn/completed" {
			return visible
		}
	}
}

func sendRPC(t *testing.T, ctx context.Context, connection *websocket.Conn, id int, method string, params any) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
}

type testRPCResponse struct {
	raw          []byte
	errorCode    int
	errorMessage string
}

func readResponse(t *testing.T, ctx context.Context, connection *websocket.Conn, id string) testRPCResponse {
	t.Helper()
	for {
		messageType, payload, err := connection.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if messageType != websocket.MessageText {
			continue
		}
		parsed, err := parseRPCMessage(payload)
		if err != nil || parsed.kind != rpcResponse || parsed.idKey != id {
			continue
		}
		response := testRPCResponse{raw: payload}
		var envelope struct {
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(payload, &envelope)
		response.errorCode = envelope.Error.Code
		response.errorMessage = envelope.Error.Message
		return response
	}
}

func readUntilMethod(t *testing.T, ctx context.Context, connection *websocket.Conn, method string) {
	t.Helper()
	for {
		visible := readVisibleRPC(t, ctx, connection)
		if visible.method == method {
			return
		}
	}
}

func readVisibleRPC(t *testing.T, ctx context.Context, connection *websocket.Conn) visibleRPC {
	t.Helper()
	for {
		messageType, payload, err := connection.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if messageType != websocket.MessageText {
			continue
		}
		parsed, err := parseRPCMessage(payload)
		if err != nil {
			continue
		}
		visible := visibleRPC{id: parsed.idKey, method: parsed.method}
		if parsed.hasError {
			var envelope struct {
				Error struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.Unmarshal(payload, &envelope)
			visible.errorCode = envelope.Error.Code
			visible.errorMessage = envelope.Error.Message
		}
		return visible
	}
}

func responseProvider(t *testing.T, payload []byte) string {
	t.Helper()
	var response struct {
		Result struct {
			ModelProvider string `json:"modelProvider"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatal(err)
	}
	return response.Result.ModelProvider
}

func assertNoInternalMessages(t *testing.T, messages []visibleRPC) {
	t.Helper()
	for _, message := range messages {
		if stringsHasPrefix(message.id, `"cps-`) || message.method == "thread/unsubscribe" {
			t.Fatalf("internal message leaked to Desktop: %#v", message)
		}
	}
}

func waitProxyDone(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not stop")
	}
}
