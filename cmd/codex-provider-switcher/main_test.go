package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/transport"
)

func TestRunHelp(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"proxy", "--help"}} {
		var stdout, stderr bytes.Buffer
		called := false
		code := run(context.Background(), "codex-provider-switcher", args, dependencies{
			getenv: func(string) string { return "" },
			stdout: &stdout,
			stderr: &stderr,
			runProxy: func(context.Context, transport.Options) error {
				called = true
				return nil
			},
		})
		if code != 0 || called || stderr.Len() != 0 {
			t.Fatalf("run(%q) = %d, called %v, stderr %q", args, code, called, stderr.String())
		}
		for _, text := range []string{"Usage:", "proxy", "--provider", "--socket", "CODEX_PROVIDER_SWITCHER_PROVIDER"} {
			if !strings.Contains(stdout.String(), text) {
				t.Errorf("help missing %q:\n%s", text, stdout.String())
			}
		}
	}
}

func TestRunVersion(t *testing.T) {
	previous := version
	version = "v1.2.3-test"
	t.Cleanup(func() { version = previous })

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), "codex-provider-switcher", []string{"--version"}, dependencies{
		getenv: func(string) string { return "" },
		stdout: &stdout,
		stderr: &stderr,
		runProxy: func(context.Context, transport.Options) error {
			return errors.New("must not run")
		},
	})
	if code != 0 || stdout.String() != "codex-provider-switcher v1.2.3-test\n" || stderr.Len() != 0 {
		t.Fatalf("run(--version) = %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestRunDirectProxyMode(t *testing.T) {
	socket := unixSocket(t)
	stdin := strings.NewReader("input")
	var stdout, stderr bytes.Buffer
	var got transport.Options
	code := run(context.Background(), "codex-provider-switcher", []string{
		"proxy", "--provider", "provider-a", "--socket", socket,
	}, dependencies{
		getenv: func(string) string { return "" },
		stdin:  stdin,
		stdout: &stdout,
		stderr: &stderr,
		runProxy: func(_ context.Context, options transport.Options) error {
			got = options
			return nil
		},
	})
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("run(proxy) = %d, stderr %q", code, stderr.String())
	}
	if got.Config.Provider != "provider-a" || got.Config.Socket != socket || got.Stdin != stdin || got.Stdout != &stdout {
		t.Fatalf("proxy options = %#v", got)
	}
}

func TestRunWrapperInterceptsAppServerProxy(t *testing.T) {
	socket := unixSocket(t)
	var got transport.Options
	delegated := false
	code := run(context.Background(), filepath.Join(t.TempDir(), "codex"), []string{
		"-c", `model="top"`, "app-server", "--enable", "feature-a", "proxy",
		"--disable=feature-b", "--sock", socket,
	}, dependencies{
		getenv: env(map[string]string{"CODEX_PROVIDER_SWITCHER_PROVIDER": "provider-b"}),
		stdin:  strings.NewReader("input"),
		stdout: io.Discard,
		stderr: io.Discard,
		runProxy: func(_ context.Context, options transport.Options) error {
			got = options
			return nil
		},
		execProcess: func(string, []string, []string) error {
			delegated = true
			return nil
		},
	})
	if code != 0 || delegated {
		t.Fatalf("run(wrapper proxy) = %d, delegated %v", code, delegated)
	}
	if got.Config.Provider != "provider-b" || got.Config.Socket != socket {
		t.Fatalf("proxy config = %#v", got.Config)
	}
}

func TestRunStockWrapperUsesDefaultSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	codexHome := t.TempDir()
	controlDir := filepath.Join(codexHome, "app-server-control")
	if err := os.MkdirAll(controlDir, 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(controlDir, "app-server-control.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}

	received := make(chan []byte, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, acceptErr := websocket.Accept(writer, request, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
			CompressionMode:    websocket.CompressionDisabled,
		})
		if acceptErr != nil {
			return
		}
		defer connection.CloseNow()
		_, payload, readErr := connection.Read(ctx)
		if readErr == nil {
			received <- payload
		}
		<-ctx.Done()
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
	})

	clientStream, switcherStream := net.Pipe()
	codeDone := make(chan int, 1)
	var stderr bytes.Buffer
	go func() {
		codeDone <- run(ctx, "codex", []string{"app-server", "proxy"}, dependencies{
			getenv: env(map[string]string{
				"CODEX_PROVIDER_SWITCHER_PROVIDER": "provider-a",
				"CODEX_HOME":                       codexHome,
			}),
			stdin:    switcherStream,
			stdout:   switcherStream,
			stderr:   &stderr,
			runProxy: transport.Run,
		})
	}()

	client, response, err := websocket.Dial(ctx, "ws://desktop.test/", &websocket.DialOptions{
		HTTPClient: singleConnHTTPClient(clientStream),
	})
	if err != nil {
		t.Fatalf("WebSocket Dial() error = %v, stderr %q", err, stderr.String())
	}
	defer client.CloseNow()
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101", response.StatusCode)
	}

	request := []byte(`{"jsonrpc":"2.0","id":1,"method":"thread/start","params":{"keep":true}}`)
	if err := client.Write(ctx, websocket.MessageText, request); err != nil {
		t.Fatal(err)
	}
	var message struct {
		Params map[string]any `json:"params"`
	}
	select {
	case payload := <-received:
		if err := json.Unmarshal(payload, &message); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("upstream did not receive rewritten request")
	}
	if message.Params["modelProvider"] != "provider-a" || message.Params["keep"] != true {
		t.Fatalf("upstream params = %#v", message.Params)
	}

	cancel()
	_ = client.CloseNow()
	select {
	case code := <-codeDone:
		if code != 0 || stderr.Len() != 0 {
			t.Fatalf("run(stock wrapper) = %d, stderr %q", code, stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stock wrapper did not stop after cancellation")
	}
}

func TestRunWrapperDelegatesProxyHelpWithoutProvider(t *testing.T) {
	current := executable(t, filepath.Join(t.TempDir(), "switcher"))
	realCodex := executable(t, filepath.Join(t.TempDir(), "codex-real"))
	delegated := false
	proxyCalled := false
	code := run(context.Background(), "codex", []string{"app-server", "proxy", "--help"}, dependencies{
		getenv: env(map[string]string{
			"CODEX_PROVIDER_SWITCHER_CODEX": realCodex,
		}),
		executable: func() (string, error) { return current, nil },
		runProxy: func(context.Context, transport.Options) error {
			proxyCalled = true
			return nil
		},
		execProcess: func(path string, argv, environment []string) error {
			delegated = path == realCodex && slices.Equal(argv, []string{realCodex, "app-server", "proxy", "--help"})
			return nil
		},
	})
	if code != 0 || !delegated || proxyCalled {
		t.Fatalf("run(proxy help) = %d, delegated %v, proxy called %v", code, delegated, proxyCalled)
	}
}

func TestRunWrapperFailsClosedWithoutProvider(t *testing.T) {
	socket := unixSocket(t)
	calledProxy := false
	delegated := false
	var stderr bytes.Buffer
	code := run(context.Background(), "codex", []string{"app-server", "proxy", "--sock", socket}, dependencies{
		getenv: func(string) string { return "" },
		stderr: &stderr,
		runProxy: func(context.Context, transport.Options) error {
			calledProxy = true
			return nil
		},
		execProcess: func(string, []string, []string) error {
			delegated = true
			return nil
		},
	})
	if code != 2 || calledProxy || delegated {
		t.Fatalf("run(missing provider) = %d, proxy %v, delegated %v", code, calledProxy, delegated)
	}
	if !strings.Contains(stderr.String(), "configuration error") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunWrapperDelegatesOtherCommands(t *testing.T) {
	current := executable(t, filepath.Join(t.TempDir(), "switcher"))
	realCodex := executable(t, filepath.Join(t.TempDir(), "codex-real"))
	environment := []string{"PATH=/usr/bin", "TEST_MARKER=preserved"}
	var gotPath string
	var gotArgv, gotEnvironment []string
	proxyCalled := false
	code := run(context.Background(), filepath.Join(t.TempDir(), "codex"), []string{"app-server", "--listen", "stdio://"}, dependencies{
		getenv:     env(map[string]string{"CODEX_PROVIDER_SWITCHER_CODEX": realCodex}),
		environ:    func() []string { return environment },
		executable: func() (string, error) { return current, nil },
		runProxy: func(context.Context, transport.Options) error {
			proxyCalled = true
			return nil
		},
		execProcess: func(path string, argv, environ []string) error {
			gotPath, gotArgv, gotEnvironment = path, argv, environ
			return nil
		},
	})
	if code != 0 || proxyCalled {
		t.Fatalf("run(delegate) = %d, proxy called %v", code, proxyCalled)
	}
	if gotPath != realCodex || !slices.Equal(gotArgv, []string{realCodex, "app-server", "--listen", "stdio://"}) || !slices.Equal(gotEnvironment, environment) {
		t.Fatalf("exec = %q, %q, %q", gotPath, gotArgv, gotEnvironment)
	}
}

func TestRunWrapperDelegatesCodexVersion(t *testing.T) {
	current := executable(t, filepath.Join(t.TempDir(), "switcher"))
	realCodex := executable(t, filepath.Join(t.TempDir(), "codex-real"))
	delegated := false
	var stdout bytes.Buffer
	code := run(context.Background(), "codex", []string{"--version"}, dependencies{
		getenv:     env(map[string]string{"CODEX_PROVIDER_SWITCHER_CODEX": realCodex}),
		executable: func() (string, error) { return current, nil },
		stdout:     &stdout,
		execProcess: func(path string, argv, environment []string) error {
			delegated = path == realCodex && slices.Equal(argv, []string{realCodex, "--version"})
			return nil
		},
	})
	if code != 0 || !delegated || stdout.Len() != 0 {
		t.Fatalf("run(codex --version) = %d, delegated %v, stdout %q", code, delegated, stdout.String())
	}
}

func TestRunReportsSanitizedFailures(t *testing.T) {
	socket := unixSocket(t)
	tests := []struct {
		name      string
		args      []string
		runProxy  func(context.Context, transport.Options) error
		wantCode  int
		wantError string
	}{
		{name: "invalid direct arguments", args: []string{"proxy", "--provider", "secret invalid"}, wantCode: 2, wantError: "configuration error"},
		{name: "transport failure", args: []string{"proxy", "--provider", "provider-a", "--socket", socket}, runProxy: func(context.Context, transport.Options) error { return errors.New("test transport failure") }, wantCode: 1, wantError: "proxy error: test transport failure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := run(context.Background(), "codex-provider-switcher", tt.args, dependencies{
				getenv:   func(string) string { return "" },
				stderr:   &stderr,
				runProxy: tt.runProxy,
			})
			if code != tt.wantCode || !strings.Contains(stderr.String(), tt.wantError) {
				t.Fatalf("run() = %d, stderr %q", code, stderr.String())
			}
			if strings.Contains(stderr.String(), "secret invalid") {
				t.Fatalf("stderr leaked argument: %q", stderr.String())
			}
		})
	}
}

func unixSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cps-main-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "app-server.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return path
}

func executable(t *testing.T, path string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func singleConnHTTPClient(connection net.Conn) *http.Client {
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
