package config

import (
	"errors"
	"flag"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseProxyFlagsOverrideEnvironment(t *testing.T) {
	flagSocket := unixSocket(t, "flag.sock")
	envSocket := unixSocket(t, "env.sock")

	result, err := ParseProxy([]string{
		"--provider", "flag-provider",
		"--socket", flagSocket,
	}, environment(map[string]string{
		"CODEX_PROVIDER_SWITCHER_PROVIDER": "env-provider",
		"CODEX_PROVIDER_SWITCHER_SOCKET":   envSocket,
	}))
	if err != nil {
		t.Fatalf("ParseProxy() error = %v", err)
	}
	want := Config{Provider: "flag-provider", Socket: flagSocket}
	if result.Config != want {
		t.Fatalf("Config = %#v, want %#v", result.Config, want)
	}
}

func TestParseProxyUsesEnvironment(t *testing.T) {
	socket := unixSocket(t, "app-server.sock")
	result, err := ParseProxy(nil, environment(map[string]string{
		"CODEX_PROVIDER_SWITCHER_PROVIDER": "provider_1.test",
		"CODEX_PROVIDER_SWITCHER_SOCKET":   socket,
	}))
	if err != nil {
		t.Fatalf("ParseProxy() error = %v", err)
	}
	want := Config{Provider: "provider_1.test", Socket: socket}
	if result.Config != want {
		t.Fatalf("Config = %#v, want %#v", result.Config, want)
	}
}

func TestParseProxyRejectsInvalidConfiguration(t *testing.T) {
	socket := unixSocket(t, "valid.sock")
	regularFile := filepath.Join(t.TempDir(), "not-a-socket")
	if err := os.WriteFile(regularFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		args       []string
		wantErr    string
		rejectText string
	}{
		{name: "missing provider", args: []string{"--socket", socket}, wantErr: "provider is required"},
		{name: "invalid provider", args: []string{"--provider", "secret bad provider", "--socket", socket}, wantErr: "invalid provider", rejectText: "secret bad provider"},
		{name: "missing socket", args: []string{"--provider", "provider-a"}, wantErr: "socket is required"},
		{name: "regular file socket", args: []string{"--provider", "provider-a", "--socket", regularFile}, wantErr: "not a Unix socket"},
		{name: "missing socket path", args: []string{"--provider", "provider-a", "--socket", filepath.Join(t.TempDir(), "missing.sock")}, wantErr: "inspect socket"},
		{name: "positional argument", args: []string{"--provider", "provider-a", "--socket", socket, "secret-positional-value"}, wantErr: "unexpected positional argument", rejectText: "secret-positional-value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseProxy(tt.args, environment(nil))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ParseProxy() error = %v, want containing %q", err, tt.wantErr)
			}
			if tt.rejectText != "" && strings.Contains(err.Error(), tt.rejectText) {
				t.Fatalf("ParseProxy() error leaked rejected value: %v", err)
			}
		})
	}
}

func TestParseProxyResolvesRelativeSocket(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	socket := unixSocket(t, filepath.Join(shortTempDir(t), "relative.sock"))
	relSocket, err := filepath.Rel(cwd, socket)
	if err != nil {
		t.Fatal(err)
	}

	result, err := ParseProxy([]string{"--provider", "provider-a", "--socket", relSocket}, environment(nil))
	if err != nil {
		t.Fatalf("ParseProxy() error = %v", err)
	}
	if result.Config.Socket != socket {
		t.Errorf("Socket = %q, want %q", result.Config.Socket, socket)
	}
}

func TestParseProxyControlModesSkipConfigurationValidation(t *testing.T) {
	result, err := ParseProxy([]string{"--version"}, environment(nil))
	if err != nil {
		t.Fatalf("ParseProxy(--version) error = %v", err)
	}
	if !result.ShowVersion {
		t.Fatal("ShowVersion = false, want true")
	}

	_, err = ParseProxy([]string{"--help"}, environment(nil))
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("ParseProxy(--help) error = %v, want flag.ErrHelp", err)
	}
}

func environment(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func unixSocket(t *testing.T, name string) string {
	t.Helper()
	path := name
	if !filepath.IsAbs(path) {
		path = filepath.Join(shortTempDir(t), path)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cps-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
