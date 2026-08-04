package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestRecoveryClientInspectsSystemErrorSubtreeAcrossPages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server := newRecoveryProtocolServer(t, ctx, "systemError", nil)

	client, err := newRecoveryClient(ctx, server.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	ids, status, err := client.inspectRecoverableSubtree(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	if status != "systemError" {
		t.Fatalf("subtree status = %q, want systemError", status)
	}
	if want := []string{"child-b", "child-a", "child-c", "root"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("subtree ids = %v, want %v", ids, want)
	}

	requests := server.requestSnapshot()
	if len(requests) != 5 || requests[0].method != "initialize" || requests[1].method != "initialized" ||
		requests[2].method != "thread/read" || requests[3].method != "thread/list" ||
		requests[4].method != "thread/list" {
		t.Fatalf("request methods = %#v", requests)
	}
	var initialize struct {
		Capabilities struct {
			ExperimentalAPI bool `json:"experimentalApi"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(requests[0].params, &initialize); err != nil || !initialize.Capabilities.ExperimentalAPI {
		t.Fatalf("initialize params = %s", requests[0].params)
	}
}

func TestRecoveryClientAcceptsOnlyExactRecoverableStatuses(t *testing.T) {
	tests := []struct {
		status string
		wantOK bool
	}{
		{status: "idle", wantOK: true},
		{status: "systemError", wantOK: true},
		{status: "active", wantOK: false},
		{status: "", wantOK: false},
		{status: "mystery", wantOK: false},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("status=%q", test.status), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			server := newRecoveryProtocolServer(t, ctx, test.status, []map[string]any{
				{"id": "child", "parentThreadId": "root"},
			})

			client, err := newRecoveryClient(ctx, server.socket)
			if err != nil {
				t.Fatal(err)
			}
			defer client.close()
			ids, status, err := client.inspectRecoverableSubtree(ctx, "root")
			if test.wantOK {
				if err != nil {
					t.Fatal(err)
				}
				if status != test.status {
					t.Fatalf("subtree status = %q, want %q", status, test.status)
				}
				if want := []string{"child", "root"}; !reflect.DeepEqual(ids, want) {
					t.Fatalf("subtree ids = %v, want %v", ids, want)
				}
				return
			}
			if err == nil {
				t.Fatalf("inspectRecoverableSubtree() error = nil for status %q", test.status)
			}
			for _, request := range server.requestSnapshot() {
				if request.method == "thread/list" {
					t.Fatalf("rejected status %q reached thread/list", test.status)
				}
			}
		})
	}
}

func TestRecoveryClientRejectsSubtreeOverLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	children := make([]map[string]any, 64)
	for index := range children {
		children[index] = map[string]any{"id": fmt.Sprintf("child-%02d", index), "parentThreadId": "root"}
	}
	server := newRecoveryProtocolServer(t, ctx, "idle", children)

	client, err := newRecoveryClient(ctx, server.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	if _, _, err := client.inspectRecoverableSubtree(ctx, "root"); err == nil {
		t.Fatal("inspectRecoverableSubtree() error = nil for 65 ids")
	}
	for _, request := range server.requestSnapshot() {
		if request.method == "thread/archive" {
			t.Fatal("oversized subtree reached thread/archive")
		}
	}
}

type recoveryProtocolRequest struct {
	method string
	params json.RawMessage
}

type recoveryProtocolServer struct {
	socket   string
	ctx      context.Context
	status   string
	children []map[string]any

	mu       sync.Mutex
	requests []recoveryProtocolRequest
}

func newRecoveryProtocolServer(t *testing.T, ctx context.Context, status string, children []map[string]any) *recoveryProtocolServer {
	t.Helper()
	server := &recoveryProtocolServer{ctx: ctx, status: status, children: children}
	server.socket = startUnixHTTPServer(t, http.HandlerFunc(server.handleUpgrade))
	return server
}

func (server *recoveryProtocolServer) handleUpgrade(writer http.ResponseWriter, request *http.Request) {
	connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	defer connection.CloseNow()
	for {
		messageType, payload, err := connection.Read(server.ctx)
		if err != nil {
			return
		}
		if messageType != websocket.MessageText {
			continue
		}
		var envelope struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(payload, &envelope) != nil {
			continue
		}
		server.mu.Lock()
		server.requests = append(server.requests, recoveryProtocolRequest{
			method: envelope.Method,
			params: append(json.RawMessage(nil), envelope.Params...),
		})
		server.mu.Unlock()
		if len(envelope.ID) == 0 {
			continue
		}
		var result any = map[string]any{}
		switch envelope.Method {
		case "initialize":
			result = map[string]any{"userAgent": "recovery-test"}
		case "thread/read":
			result = map[string]any{"thread": map[string]any{
				"id": "root", "status": map[string]any{"type": server.status},
			}}
		case "thread/list":
			data := server.children
			nextCursor := any(nil)
			if data == nil {
				var params struct {
					Cursor *string `json:"cursor"`
				}
				_ = json.Unmarshal(envelope.Params, &params)
				if params.Cursor == nil {
					data = []map[string]any{
						{"id": "child-a", "parentThreadId": "root"},
						{"id": "child-b", "parentThreadId": "child-a"},
					}
					nextCursor = "page-2"
				} else {
					data = []map[string]any{{"id": "child-c", "parentThreadId": "root"}}
				}
			}
			result = map[string]any{"data": data, "nextCursor": nextCursor}
		}
		response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": envelope.ID, "result": result})
		_ = connection.Write(server.ctx, websocket.MessageText, response)
	}
}

func (server *recoveryProtocolServer) requestSnapshot() []recoveryProtocolRequest {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]recoveryProtocolRequest(nil), server.requests...)
}
