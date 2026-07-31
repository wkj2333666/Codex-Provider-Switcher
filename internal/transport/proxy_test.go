package transport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/config"
)

func TestRunSupportsAllPayloadLengthEncodings(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	received := make(chan []byte, 3)
	socket := startWebSocketServer(t, func(connection *websocket.Conn) {
		for range 3 {
			_, payload, err := connection.Read(ctx)
			if err != nil {
				return
			}
			received <- payload
		}
		<-ctx.Done()
	})
	client, done := dialTestProxy(t, ctx, socket, Options{})
	defer client.CloseNow()

	for _, size := range []int{125, 126, 65536} {
		message := jsonMessageOfLength(t, size)
		if err := client.Write(ctx, websocket.MessageText, message); err != nil {
			t.Fatalf("Write(%d bytes) error = %v", size, err)
		}
		if got := <-received; !bytes.Equal(got, message) {
			t.Fatalf("received %d-byte payload differs", size)
		}
	}
	stopProxy(t, cancel, client, done)
}

func TestRunReassemblesFragmentedClientText(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	received := make(chan receivedMessage, 1)
	socket := startWebSocketServer(t, func(connection *websocket.Conn) {
		messageType, payload, err := connection.Read(ctx)
		received <- receivedMessage{messageType: messageType, payload: payload, err: err}
		<-ctx.Done()
	})
	client, done := dialTestProxy(t, ctx, socket, Options{})
	defer client.CloseNow()

	writer, err := client.Writer(ctx, websocket.MessageText)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"thread/`,
		`resume","params":{"threadId":"abc",`,
		`"modelProvider":"wrong"}}`,
	} {
		if _, err := io.WriteString(writer, fragment); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	message := <-received
	if message.err != nil || message.messageType != websocket.MessageText {
		t.Fatalf("upstream message = %v, %v", message.messageType, message.err)
	}
	var decoded struct {
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(message.payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Params["threadId"] != "abc" || decoded.Params["modelProvider"] != "provider-a" {
		t.Fatalf("rewritten params = %#v", decoded.Params)
	}
	stopProxy(t, cancel, client, done)
}

func TestRunPassesBinaryMessagesBothDirections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	received := make(chan receivedMessage, 1)
	serverPayload := []byte{0x00, 0xff, 0x10, 0x80}
	socket := startWebSocketServer(t, func(connection *websocket.Conn) {
		messageType, payload, err := connection.Read(ctx)
		received <- receivedMessage{messageType: messageType, payload: payload, err: err}
		if err == nil {
			_ = connection.Write(ctx, websocket.MessageBinary, serverPayload)
		}
		<-ctx.Done()
	})
	client, done := dialTestProxy(t, ctx, socket, Options{})
	defer client.CloseNow()

	clientPayload := []byte{0xde, 0xad, 0xbe, 0xef}
	if err := client.Write(ctx, websocket.MessageBinary, clientPayload); err != nil {
		t.Fatal(err)
	}
	upstream := <-received
	if upstream.err != nil || upstream.messageType != websocket.MessageBinary || !bytes.Equal(upstream.payload, clientPayload) {
		t.Fatalf("upstream binary = %v %x, error %v", upstream.messageType, upstream.payload, upstream.err)
	}
	messageType, payload, err := client.Read(ctx)
	if err != nil || messageType != websocket.MessageBinary || !bytes.Equal(payload, serverPayload) {
		t.Fatalf("downstream binary = %v %x, error %v", messageType, payload, err)
	}
	stopProxy(t, cancel, client, done)
}

func TestRunHandlesPingPong(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	socket := startWebSocketServer(t, func(connection *websocket.Conn) { <-ctx.Done() })
	client, done := dialTestProxy(t, ctx, socket, Options{})
	defer client.CloseNow()

	readDone := make(chan error, 1)
	go func() {
		_, _, err := client.Read(ctx)
		readDone <- err
	}()
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
	stopProxy(t, cancel, client, done)
	<-readDone
}

func TestRunRejectsMessageAboveLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	socket := startWebSocketServer(t, func(connection *websocket.Conn) { <-ctx.Done() })
	client, done := dialTestProxy(t, ctx, socket, Options{MaxMessageSize: 64})
	defer client.CloseNow()

	if err := client.Write(ctx, websocket.MessageText, jsonMessageOfLength(t, 126)); err != nil {
		t.Fatal(err)
	}
	_, _, readErr := client.Read(ctx)
	if websocket.CloseStatus(readErr) != websocket.StatusMessageTooBig {
		t.Fatalf("CloseStatus = %v, want %v (error %v)", websocket.CloseStatus(readErr), websocket.StatusMessageTooBig, readErr)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "forwarding failed") {
			t.Fatalf("Run() error = %v, want forwarding failure", err)
		}
	case <-ctx.Done():
		t.Fatal("Run() did not stop after oversized message")
	}
}

func TestRunHandlesShortStdioReadsAndWrites(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	received := make(chan []byte, 1)
	socket := startWebSocketServer(t, func(connection *websocket.Conn) {
		_, payload, err := connection.Read(ctx)
		if err == nil {
			received <- payload
			_ = connection.Write(ctx, websocket.MessageText, []byte(`{"id":1,"result":"ok"}`))
		}
		<-ctx.Done()
	})
	clientStream, switcherStream := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Config: config.Config{Provider: "provider-a", Socket: socket},
			Stdin:  chunkReader{reader: switcherStream, maximum: 3},
			Stdout: chunkWriter{writer: switcherStream, maximum: 2},
		})
	}()
	client, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: pipeHTTPClient(clientStream)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	request := []byte(`{"method":"thread/fork","params":{"threadId":"abc"}}`)
	if err := client.Write(ctx, websocket.MessageText, request); err != nil {
		t.Fatal(err)
	}
	if payload := <-received; !bytes.Contains(payload, []byte(`"modelProvider":"provider-a"`)) {
		t.Fatalf("upstream payload = %s", payload)
	}
	messageType, payload, err := client.Read(ctx)
	if err != nil || messageType != websocket.MessageText || string(payload) != `{"id":1,"result":"ok"}` {
		t.Fatalf("downstream message = %v %q, error %v", messageType, payload, err)
	}
	stopProxy(t, cancel, client, done)
}

func TestRunForwardsHeadersPathAndSubprotocolWithoutCompression(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	type handshakeDetails struct {
		path       string
		host       string
		testHeader string
		extension  string
		hopHeaders []string
	}
	handshake := make(chan handshakeDetails, 1)
	socket := startUnixHTTPServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
			Subprotocols:       []string{"codex-v2"},
			InsecureSkipVerify: true,
			CompressionMode:    websocket.CompressionDisabled,
		})
		if err != nil {
			return
		}
		defer connection.CloseNow()
		handshake <- handshakeDetails{
			path:       request.URL.RequestURI(),
			host:       request.Host,
			testHeader: request.Header.Get("X-Codex-Test"),
			extension:  request.Header.Get("Sec-WebSocket-Extensions"),
			hopHeaders: []string{
				request.Header.Get("Keep-Alive"),
				request.Header.Get("Proxy-Connection"),
				request.Header.Get("Te"),
				request.Header.Get("Trailer"),
			},
		}
		<-ctx.Done()
	}))

	clientStream, switcherStream := net.Pipe()
	proxyDone := make(chan error, 1)
	go func() {
		proxyDone <- Run(ctx, Options{
			Config: config.Config{Provider: "provider-a", Socket: socket},
			Stdin:  switcherStream,
			Stdout: switcherStream,
		})
	}()

	client, response, err := websocket.Dial(ctx, "ws://desktop.test/rpc?connection=1", &websocket.DialOptions{
		HTTPClient: pipeHTTPClient(clientStream),
		HTTPHeader: http.Header{
			"X-Codex-Test":     []string{"retained"},
			"Keep-Alive":       []string{"timeout=5"},
			"Proxy-Connection": []string{"keep-alive"},
			"Te":               []string{"trailers"},
			"Trailer":          []string{"X-Trailer"},
		},
		Subprotocols:    []string{"other", "codex-v2"},
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		t.Fatalf("WebSocket Dial() error = %v", err)
	}
	defer client.CloseNow()

	details := <-handshake
	if details.path != "/rpc?connection=1" || details.host != "desktop.test" || details.testHeader != "retained" {
		t.Fatalf("upstream handshake = %#v", details)
	}
	if details.extension != "" {
		t.Fatalf("upstream negotiated extension request = %q", details.extension)
	}
	for index, got := range details.hopHeaders {
		if got != "" {
			t.Fatalf("upstream hop-by-hop header %d = %q, want empty", index, got)
		}
	}
	if got := response.Header.Get("Sec-WebSocket-Extensions"); got != "" {
		t.Fatalf("downstream extension = %q, want empty", got)
	}
	if got := client.Subprotocol(); got != "codex-v2" {
		t.Fatalf("downstream subprotocol = %q, want codex-v2", got)
	}

	cancel()
	_ = client.CloseNow()
	select {
	case <-proxyDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not stop after cancellation")
	}
}

func TestRunRejectsPlainHTTPWithoutDialingUpstream(t *testing.T) {
	response, runErr := runRawRequest(t, filepath.Join(t.TempDir(), "missing.sock"), "GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
	if runErr == nil || !strings.Contains(runErr.Error(), "invalid downstream") {
		t.Fatalf("Run() error = %v, want invalid downstream error", runErr)
	}
}

func TestRunReturnsBadGatewayBeforeUpgradeWhenUpstreamFails(t *testing.T) {
	request := "GET / HTTP/1.1\r\n" +
		"Host: localhost\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
	response, runErr := runRawRequest(t, filepath.Join(t.TempDir(), "missing.sock"), request)
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", response.StatusCode)
	}
	if runErr == nil || !strings.Contains(runErr.Error(), "upstream WebSocket handshake failed") {
		t.Fatalf("Run() error = %v, want upstream handshake error", runErr)
	}
}

func TestRunPropagatesUpstreamClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	serverClose := make(chan error, 1)
	upstreamReady := make(chan struct{})
	socket := startWebSocketServer(t, func(connection *websocket.Conn) {
		_, _, _ = connection.Read(ctx)
		close(upstreamReady)
		serverClose <- connection.Close(websocket.StatusGoingAway, "server restart")
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

	client, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{
		HTTPClient: pipeHTTPClient(clientStream),
	})
	if err != nil {
		t.Fatalf("WebSocket Dial() error = %v", err)
	}
	defer client.CloseNow()
	if err := client.Write(ctx, websocket.MessageText, []byte(`{"method":"custom/ready"}`)); err != nil {
		t.Fatal(err)
	}
	<-upstreamReady

	_, _, readErr := client.Read(ctx)
	if err := <-serverClose; err != nil {
		t.Fatalf("upstream server Close() error = %v", err)
	}
	if websocket.CloseStatus(readErr) != websocket.StatusGoingAway {
		t.Fatalf("client CloseStatus = %v, want %v (error %v)", websocket.CloseStatus(readErr), websocket.StatusGoingAway, readErr)
	}
	var closeErr websocket.CloseError
	if !errors.As(readErr, &closeErr) || closeErr.Reason != "server restart" {
		t.Fatalf("client close error = %#v, want reason %q", readErr, "server restart")
	}

	select {
	case err := <-proxyDone:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Run() did not stop after upstream close")
	}
}

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
	return startUnixHTTPServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
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
	}))
}

func startUnixHTTPServer(t *testing.T, handler http.Handler) string {
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

	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
	})
	return socket
}

func runRawRequest(t *testing.T, socket, request string) (*http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientStream, switcherStream := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Config: config.Config{Provider: "provider-a", Socket: socket},
			Stdin:  switcherStream,
			Stdout: switcherStream,
		})
	}()
	if _, err := io.WriteString(clientStream, request); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(clientStream), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	_ = clientStream.Close()
	select {
	case runErr := <-done:
		return response, runErr
	case <-ctx.Done():
		t.Fatal("Run() did not return after HTTP error response")
		return nil, nil
	}
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

func dialTestProxy(t *testing.T, ctx context.Context, socket string, options Options) (*websocket.Conn, <-chan error) {
	t.Helper()
	clientStream, switcherStream := net.Pipe()
	done := make(chan error, 1)
	options.Config = config.Config{Provider: "provider-a", Socket: socket}
	options.Stdin = switcherStream
	options.Stdout = switcherStream
	go func() { done <- Run(ctx, options) }()
	client, response, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: pipeHTTPClient(clientStream)})
	if err != nil {
		t.Fatalf("WebSocket Dial() error = %v", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101", response.StatusCode)
	}
	return client, done
}

func stopProxy(t *testing.T, cancel context.CancelFunc, client *websocket.Conn, done <-chan error) {
	t.Helper()
	cancel()
	_ = client.CloseNow()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not stop after cancellation")
	}
}

func jsonMessageOfLength(t *testing.T, length int) []byte {
	t.Helper()
	prefix := `{"method":"custom/echo","padding":"`
	suffix := `"}`
	padding := length - len(prefix) - len(suffix)
	if padding < 0 {
		t.Fatalf("message length %d is too small", length)
	}
	message := []byte(prefix + strings.Repeat("x", padding) + suffix)
	if len(message) != length || !json.Valid(message) {
		t.Fatalf("generated message length = %d, valid = %v", len(message), json.Valid(message))
	}
	return message
}

type chunkReader struct {
	reader  io.Reader
	maximum int
}

func (reader chunkReader) Read(destination []byte) (int, error) {
	if len(destination) > reader.maximum {
		destination = destination[:reader.maximum]
	}
	return reader.reader.Read(destination)
}

type chunkWriter struct {
	writer  io.Writer
	maximum int
}

func (writer chunkWriter) Write(source []byte) (int, error) {
	if len(source) > writer.maximum {
		source = source[:writer.maximum]
	}
	return writer.writer.Write(source)
}
