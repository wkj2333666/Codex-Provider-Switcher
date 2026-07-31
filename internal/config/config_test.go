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

func TestParseFlagsOverrideEnvironment(t *testing.T) {
	flagSocket := unixSocket(t, "flag.sock")
	envSocket := unixSocket(t, "env.sock")
	flagCodex := executable(t, "flag-codex")
	envCodex := executable(t, "env-codex")
	env := environment(map[string]string{
		"CODEX_PROVIDER_SWITCHER_PROVIDER": "env-provider",
		"CODEX_PROVIDER_SWITCHER_SOCKET":   envSocket,
		"CODEX_PROVIDER_SWITCHER_CODEX":    envCodex,
	})

	result, err := Parse([]string{
		"--provider", "flag-provider",
		"--socket", flagSocket,
		"--codex", flagCodex,
	}, env)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if result.Config.Provider != "flag-provider" {
		t.Errorf("Provider = %q", result.Config.Provider)
	}
	if result.Config.Socket != flagSocket {
		t.Errorf("Socket = %q, want %q", result.Config.Socket, flagSocket)
	}
	if result.Config.Codex != flagCodex {
		t.Errorf("Codex = %q, want %q", result.Config.Codex, flagCodex)
	}
}

func TestParseUsesEnvironmentAndDefaultCodex(t *testing.T) {
	socket := unixSocket(t, "app-server.sock")
	binDir := t.TempDir()
	codex := executableAt(t, filepath.Join(binDir, "codex"))
	t.Setenv("PATH", binDir)

	result, err := Parse(nil, environment(map[string]string{
		"CODEX_PROVIDER_SWITCHER_PROVIDER": "provider_1.test",
		"CODEX_PROVIDER_SWITCHER_SOCKET":   socket,
	}))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if result.Config.Provider != "provider_1.test" {
		t.Errorf("Provider = %q", result.Config.Provider)
	}
	if result.Config.Codex != codex {
		t.Errorf("Codex = %q, want %q", result.Config.Codex, codex)
	}
}

func TestParseRejectsInvalidConfiguration(t *testing.T) {
	socket := unixSocket(t, "valid.sock")
	codex := executable(t, "codex")
	regularFile := filepath.Join(t.TempDir(), "not-a-socket")
	if err := os.WriteFile(regularFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	nonExecutable := filepath.Join(t.TempDir(), "not-executable")
	if err := os.WriteFile(nonExecutable, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		args       []string
		wantErr    string
		rejectText string
	}{
		{name: "missing provider", args: []string{"--socket", socket, "--codex", codex}, wantErr: "provider is required"},
		{name: "invalid provider", args: []string{"--provider", "bad provider", "--socket", socket, "--codex", codex}, wantErr: "invalid provider"},
		{name: "missing socket", args: []string{"--provider", "provider-a", "--codex", codex}, wantErr: "socket is required"},
		{name: "regular file socket", args: []string{"--provider", "provider-a", "--socket", regularFile, "--codex", codex}, wantErr: "not a Unix socket"},
		{name: "missing socket path", args: []string{"--provider", "provider-a", "--socket", filepath.Join(t.TempDir(), "missing.sock"), "--codex", codex}, wantErr: "inspect socket"},
		{name: "non executable codex", args: []string{"--provider", "provider-a", "--socket", socket, "--codex", nonExecutable}, wantErr: "not executable"},
		{name: "positional argument", args: []string{"--provider", "provider-a", "--socket", socket, "--codex", codex, "secret-positional-value"}, wantErr: "unexpected positional argument", rejectText: "secret-positional-value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.args, environment(nil))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Parse() error = %v, want containing %q", err, tt.wantErr)
			}
			if tt.rejectText != "" && strings.Contains(err.Error(), tt.rejectText) {
				t.Fatalf("Parse() error leaked rejected value: %v", err)
			}
		})
	}
}

func TestParseResolvesRelativeExplicitPaths(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	socket := unixSocket(t, filepath.Join(dir, "relative.sock"))
	codex := executableAt(t, filepath.Join(dir, "codex-bin"))
	relSocket, err := filepath.Rel(cwd, socket)
	if err != nil {
		t.Fatal(err)
	}
	relCodex, err := filepath.Rel(cwd, codex)
	if err != nil {
		t.Fatal(err)
	}

	result, err := Parse([]string{"--provider", "provider-a", "--socket", relSocket, "--codex", relCodex}, environment(nil))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if result.Config.Socket != socket || result.Config.Codex != codex {
		t.Errorf("Config = %#v, want socket %q and codex %q", result.Config, socket, codex)
	}
}

func TestParseControlModesSkipConfigurationValidation(t *testing.T) {
	result, err := Parse([]string{"--version"}, environment(nil))
	if err != nil {
		t.Fatalf("Parse(--version) error = %v", err)
	}
	if !result.ShowVersion {
		t.Fatal("ShowVersion = false, want true")
	}

	_, err = Parse([]string{"--help"}, environment(nil))
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("Parse(--help) error = %v, want flag.ErrHelp", err)
	}
}

func environment(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func unixSocket(t *testing.T, name string) string {
	t.Helper()
	path := name
	if !filepath.IsAbs(path) {
		path = filepath.Join(t.TempDir(), path)
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

func executable(t *testing.T, name string) string {
	t.Helper()
	return executableAt(t, filepath.Join(t.TempDir(), name))
}

func executableAt(t *testing.T, path string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}
