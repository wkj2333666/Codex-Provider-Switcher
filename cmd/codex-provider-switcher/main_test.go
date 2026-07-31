package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wkj2333666/Codex-Provider-Switcher/internal/proxy"
)

func TestRunHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	called := false
	code := run([]string{"--help"}, dependencies{
		getenv: func(string) string { return "" },
		stdin:  strings.NewReader(""),
		stdout: &stdout,
		stderr: &stderr,
		runProxy: func(proxy.Options) error {
			called = true
			return nil
		},
	})
	if code != 0 {
		t.Errorf("run(--help) = %d, want 0", code)
	}
	if called {
		t.Fatal("proxy was started for --help")
	}
	for _, text := range []string{"Usage:", "--provider", "--socket", "--codex", "CODEX_PROVIDER_SWITCHER_PROVIDER"} {
		if !strings.Contains(stdout.String(), text) {
			t.Errorf("help missing %q:\n%s", text, stdout.String())
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunVersion(t *testing.T) {
	previous := version
	version = "v1.2.3-test"
	t.Cleanup(func() { version = previous })

	var stdout, stderr bytes.Buffer
	code := run([]string{"--version"}, dependencies{
		getenv:   func(string) string { return "" },
		stdin:    strings.NewReader(""),
		stdout:   &stdout,
		stderr:   &stderr,
		runProxy: func(proxy.Options) error { return errors.New("must not run") },
	})
	if code != 0 || stdout.String() != "codex-provider-switcher v1.2.3-test\n" || stderr.Len() != 0 {
		t.Fatalf("run(--version) = %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestRunRejectsConfigurationBeforeStartingProxy(t *testing.T) {
	var stderr bytes.Buffer
	called := false
	code := run([]string{"--provider", "secret invalid provider"}, dependencies{
		getenv: func(string) string { return "" },
		stdin:  strings.NewReader(""),
		stdout: io.Discard,
		stderr: &stderr,
		runProxy: func(proxy.Options) error {
			called = true
			return nil
		},
	})
	if code != 2 {
		t.Errorf("run(invalid config) = %d, want 2", code)
	}
	if called {
		t.Fatal("proxy was started with invalid configuration")
	}
	if !strings.Contains(stderr.String(), "configuration error") || strings.Contains(stderr.String(), "secret invalid provider") {
		t.Errorf("unsafe configuration diagnostic: %q", stderr.String())
	}
}

func TestRunPassesResolvedConfigurationAndStreams(t *testing.T) {
	socketPath := filepath.Join(shortTempDir(t), "app-server.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	executable := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	stdin := strings.NewReader("input")
	var stdout, stderr bytes.Buffer
	signals := make(chan os.Signal)
	var got proxy.Options
	code := run([]string{"--provider", "provider-a", "--socket", socketPath, "--codex", executable}, dependencies{
		getenv:  func(string) string { return "" },
		stdin:   stdin,
		stdout:  &stdout,
		stderr:  &stderr,
		signals: signals,
		runProxy: func(options proxy.Options) error {
			got = options
			return nil
		},
	})
	if code != 0 {
		t.Fatalf("run(valid config) = %d, stderr %q", code, stderr.String())
	}
	if got.Config.Provider != "provider-a" || got.Config.Socket != socketPath || got.Config.Codex != executable {
		t.Errorf("proxy config = %#v", got.Config)
	}
	if got.Stdin != stdin || got.Stdout != &stdout || got.Stderr != &stderr || got.Signals != signals {
		t.Error("run did not pass through process streams and signal channel")
	}
}

func TestRunReportsProxyFailure(t *testing.T) {
	socketPath := filepath.Join(shortTempDir(t), "app-server.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	executable := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	code := run([]string{"--provider", "provider-a", "--socket", socketPath, "--codex", executable}, dependencies{
		getenv:   func(string) string { return "" },
		stdin:    strings.NewReader(""),
		stdout:   io.Discard,
		stderr:   &stderr,
		runProxy: func(proxy.Options) error { return errors.New("test process failure") },
	})
	if code != 1 || !strings.Contains(stderr.String(), "proxy error: test process failure") {
		t.Fatalf("run(proxy failure) = %d, stderr %q", code, stderr.String())
	}
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
