package transport

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/config"
)

func TestRunUpgradesAndRewritesOverUnixSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	received := make(chan receivedMessage, 1)
	socket := startWebSocketServer(t, func(connection *websocket.Conn) {
		messageType, payload, err := connection.Read(ctx)
		received <- receivedMessage{messageType: messageType, payload: payload, err: err}
		if err != nil {
			return
		}
		_ = connection.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
		<-ctx.Done()
	})

	clientStream, switcherStream := net.Pipe()
	proxyDone := make(chan error, 1)
	go func() {
		proxyDone <- Run(ctx, Options{
			Config: config.Config{Provider: "provider-a", Socket: socket},
			Stdin:  switcherStream,
			Stdout: switcherStream,
		})
	}()

	client, response, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{
		HTTPClient: pipeHTTPClient(clientStream),
	})
	if err != nil {
		t.Fatalf("WebSocket Dial() error = %v", err)
	}
	defer client.CloseNow()
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101", response.StatusCode)
	}

	request := []byte(`{"jsonrpc":"2.0","id":1,"method":"thread/start","params":{"keep":true}}`)
	if err := client.Write(ctx, websocket.MessageText, request); err != nil {
		t.Fatalf("client Write() error = %v", err)
	}

	upstream := <-received
	if upstream.err != nil {
		t.Fatalf("upstream Read() error = %v", upstream.err)
	}
	if upstream.messageType != websocket.MessageText {
		t.Fatalf("upstream message type = %v, want text", upstream.messageType)
	}
	var message struct {
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(upstream.payload, &message); err != nil {
		t.Fatalf("upstream payload is invalid JSON: %v", err)
	}
	if message.Params["modelProvider"] != "provider-a" || message.Params["keep"] != true {
		t.Fatalf("upstream params = %#v", message.Params)
	}

	messageType, payload, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("client Read() error = %v", err)
	}
	wantResponse := `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`
	if messageType != websocket.MessageText || string(payload) != wantResponse {
		t.Fatalf("downstream message = %v %q, want text %q", messageType, payload, wantResponse)
	}

	cancel()
	_ = client.CloseNow()
	select {
	case <-proxyDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not stop after cancellation")
	}
}

type receivedMessage struct {
	messageType websocket.MessageType
	payload     []byte
	err         error
}

func startWebSocketServer(t *testing.T, handle func(*websocket.Conn)) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cps-transport-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "app-server.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}

	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, acceptErr := websocket.Accept(writer, request, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
			CompressionMode:    websocket.CompressionDisabled,
		})
		if acceptErr != nil {
			return
		}
		defer connection.CloseNow()
		connection.SetReadLimit(64 << 20)
		handle(connection)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
	})
	return socket
}

func pipeHTTPClient(connection net.Conn) *http.Client {
	used := false
	return &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			if used {
				return nil, io.EOF
			}
			used = true
			return connection, nil
		},
	}}
}
