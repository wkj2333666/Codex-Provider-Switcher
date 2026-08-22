// Package transport proxies WebSocket messages between Desktop stdio and a
// Codex app-server Unix socket.
package transport

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/config"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/handoff"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/modelroute"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/recovery"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/selection"
)

const maxMessageSize int64 = 64 << 20

var errRoutingPolicy = errors.New("routing policy rejected message")

// Options supplies validated configuration and process streams.
type Options struct {
	Config         config.Config
	Stdin          io.Reader
	Stdout         io.Writer
	MaxMessageSize int64
}

// Run accepts one downstream HTTP Upgrade and proxies its WebSocket messages.
func Run(ctx context.Context, options Options) error {
	if ctx == nil {
		ctx = context.Background()
	}
	stdin := options.Stdin
	if stdin == nil {
		stdin = io.Reader(nil)
	}
	stdout := options.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	if stdin == nil {
		return errors.New("downstream input is unavailable")
	}

	connection := newStreamConn(stdin, stdout)
	listener := newSingleConnListener(connection)
	defer listener.Close()
	defer connection.Close()

	result := make(chan error, 1)
	requestStarted := make(chan struct{})
	var requestOnce sync.Once
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestOnce.Do(func() { close(requestStarted) })
		result <- serveConnection(ctx, writer, request, options)
	})
	server := &http.Server{
		Handler:  handler,
		ErrorLog: log.New(io.Discard, "", 0),
	}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return nil
	case <-connection.closed:
		select {
		case <-requestStarted:
			select {
			case err := <-result:
				return err
			case <-ctx.Done():
				return nil
			}
		default:
			return errors.New("downstream closed before WebSocket upgrade")
		}
	}
}

func serveConnection(ctx context.Context, writer http.ResponseWriter, request *http.Request, options Options) error {
	if !validUpgrade(request) {
		writeHTTPError(writer, http.StatusBadRequest)
		return errors.New("invalid downstream WebSocket upgrade")
	}
	stateDirectory := options.Config.StateDir
	if stateDirectory == "" {
		stateDirectory = filepath.Join(filepath.Dir(options.Config.Socket), ".codex-provider-switcher")
	}
	routes, err := modelroute.Load(stateDirectory)
	if err != nil {
		writeHTTPError(writer, http.StatusInternalServerError)
		return errors.New("provider model routes unavailable")
	}
	selections, err := selection.Open(stateDirectory)
	if err != nil {
		writeHTTPError(writer, http.StatusInternalServerError)
		return errors.New("provider selection state unavailable")
	}
	recoveryStore, err := recovery.Open(stateDirectory)
	if err != nil {
		writeHTTPError(writer, http.StatusInternalServerError)
		return errors.New("provider recovery state unavailable")
	}

	upstream, _, err := dialUpstream(ctx, request, options.Config.Socket)
	if err != nil {
		writeHTTPError(writer, http.StatusBadGateway)
		return errors.New("upstream WebSocket handshake failed")
	}
	defer upstream.CloseNow()

	selectedSubprotocol := upstream.Subprotocol()
	acceptOptions := &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	}
	if selectedSubprotocol != "" {
		acceptOptions.Subprotocols = []string{selectedSubprotocol}
	}
	downstream, err := websocket.Accept(writer, request, acceptOptions)
	if err != nil {
		return errors.New("downstream WebSocket handshake failed")
	}
	defer downstream.CloseNow()

	limit := options.MaxMessageSize
	if limit <= 0 {
		limit = maxMessageSize
	}
	downstream.SetReadLimit(limit)
	upstream.SetReadLimit(limit)
	return bridge(
		ctx, downstream, upstream, options.Config.Provider, options.Config.CodexHome, options.Config.Socket,
		routes, selections, recoveryStore,
	)
}

func writeHTTPError(writer http.ResponseWriter, status int) {
	writer.Header().Set("Content-Length", "0")
	writer.WriteHeader(status)
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

func dialUpstream(ctx context.Context, request *http.Request, socket string) (*websocket.Conn, *http.Response, error) {
	headers := request.Header.Clone()
	for _, name := range headerTokens(request.Header.Values("Connection")) {
		headers.Del(name)
	}
	for _, name := range []string{
		"Connection",
		"Host",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Proxy-Connection",
		"TE",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
		"Sec-WebSocket-Key",
		"Sec-WebSocket-Version",
		"Sec-WebSocket-Extensions",
		"Sec-WebSocket-Protocol",
	} {
		headers.Del(name)
	}

	transport := &http.Transport{
		DisableCompression: true,
		DialContext: func(dialContext context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(dialContext, "unix", socket)
		},
	}
	client := &http.Client{Transport: transport}
	path := request.URL.RequestURI()
	if path == "" {
		path = "/"
	}
	return websocket.Dial(ctx, "ws://localhost"+path, &websocket.DialOptions{
		HTTPClient:      client,
		HTTPHeader:      headers,
		Host:            request.Host,
		Subprotocols:    headerTokens(request.Header.Values("Sec-WebSocket-Protocol")),
		CompressionMode: websocket.CompressionDisabled,
	})
}

func bridge(
	ctx context.Context,
	downstream, upstream *websocket.Conn,
	provider, codexHome, socket string,
	routes *modelroute.Catalog,
	selections providerSelections,
	recoveries recoveryJournals,
) error {
	bridgeContext, cancel := context.WithCancel(ctx)
	defer cancel()
	current, err := newSessionState(
		provider, routes,
		socket,
		func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
			return upstream.Write(ctx, messageType, payload)
		},
		func(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
			return downstream.Write(ctx, messageType, payload)
		},
	)
	if err != nil {
		return errors.New("initialize provider handoff session")
	}
	current.codexHome = codexHome
	current.selections = selections
	current.recoveries = recoveries
	coordinator, err := handoff.Open(socket, current)
	if err != nil {
		current.closeState()
		return errors.New("initialize provider handoff coordinator")
	}
	current.coordinator = coordinator
	defer current.closeState()
	defer coordinator.Close()

	type pumpResult struct {
		err         error
		destination *websocket.Conn
	}
	results := make(chan pumpResult, 2)
	go func() {
		err := current.pumpDownstream(bridgeContext, downstream)
		results <- pumpResult{destination: upstream, err: err}
	}()
	go func() {
		err := current.pumpUpstream(bridgeContext, upstream)
		results <- pumpResult{destination: downstream, err: err}
	}()

	first := <-results
	if status := websocket.CloseStatus(first.err); status != -1 {
		var closeError websocket.CloseError
		reason := ""
		if errors.As(first.err, &closeError) {
			reason = closeError.Reason
		}
		if status == websocket.StatusNoStatusRcvd {
			status = websocket.StatusNormalClosure
		}
		_ = first.destination.Close(status, reason)
		cancel()
		_ = downstream.CloseNow()
		_ = upstream.CloseNow()
		select {
		case <-results:
		case <-time.After(time.Second):
		}
		return nil
	}

	if ctx.Err() != nil {
		cancel()
		_ = downstream.CloseNow()
		_ = upstream.CloseNow()
		return nil
	}

	status := websocket.StatusInternalError
	reason := "proxy forwarding failed"
	if errors.Is(first.err, errRoutingPolicy) {
		status = websocket.StatusPolicyViolation
		reason = "routing policy rejected message"
	}
	closeConnections(status, reason, downstream, upstream)
	cancel()
	_ = downstream.CloseNow()
	_ = upstream.CloseNow()
	select {
	case <-results:
	case <-time.After(time.Second):
	}

	if first.err != nil {
		return errors.New("WebSocket message forwarding failed")
	}
	return nil
}

func closeConnections(status websocket.StatusCode, reason string, connections ...*websocket.Conn) {
	done := make(chan struct{}, len(connections))
	for _, connection := range connections {
		go func(connection *websocket.Conn) {
			_ = connection.Close(status, reason)
			done <- struct{}{}
		}(connection)
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for range connections {
		select {
		case <-done:
		case <-timer.C:
			return
		}
	}
}

func validUpgrade(request *http.Request) bool {
	if request.Method != http.MethodGet || !request.ProtoAtLeast(1, 1) {
		return false
	}
	if !containsToken(request.Header.Values("Connection"), "upgrade") ||
		!containsToken(request.Header.Values("Upgrade"), "websocket") ||
		request.Header.Get("Sec-WebSocket-Version") != "13" {
		return false
	}
	keys := request.Header.Values("Sec-WebSocket-Key")
	if len(keys) != 1 {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keys[0]))
	return err == nil && len(decoded) == 16
}

func containsToken(values []string, target string) bool {
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), target) {
				return true
			}
		}
	}
	return false
}

func headerTokens(values []string) []string {
	var tokens []string
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			token = strings.TrimSpace(token)
			if token != "" {
				tokens = append(tokens, token)
			}
		}
	}
	return tokens
}
