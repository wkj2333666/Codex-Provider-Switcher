package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wkj2333666/Codex-Provider-Switcher/internal/config"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("CODEX_PROVIDER_SWITCHER_TEST_HELPER"); mode != "" {
		runHelper(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestRunForwardsRewrittenInputAndUntouchedOutput(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_PROVIDER_SWITCHER_TEST_HELPER", "echo")

	input := `{"jsonrpc":"2.0","id":1,"method":"thread/start","params":{"keep":true}}` + "\n"
	var stdout, stderr bytes.Buffer
	err = Run(Options{
		Config: config.Config{Provider: "provider-a", Socket: "/tmp/app-server.sock", Codex: executable},
		Stdin:  strings.NewReader(input),
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(stdout.String(), `"modelProvider":"provider-a"`) || !strings.Contains(stdout.String(), `"keep":true`) {
		t.Errorf("stdout = %q, want rewritten request", stdout.String())
	}
	wantArgs := "app-server proxy --sock /tmp/app-server.sock\n"
	if stderr.String() != wantArgs {
		t.Errorf("stderr = %q, want exact child diagnostic %q", stderr.String(), wantArgs)
	}
}

func TestRunPropagatesChildExitCode(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_PROVIDER_SWITCHER_TEST_HELPER", "exit")
	t.Setenv("CODEX_PROVIDER_SWITCHER_TEST_EXIT", "23")

	err = Run(Options{
		Config: config.Config{Provider: "provider-a", Socket: "/tmp/app-server.sock", Codex: executable},
		Stdin:  strings.NewReader(""),
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	if code := ExitCode(err); code != 23 {
		t.Fatalf("ExitCode(Run()) = %d, want 23 (error %v)", code, err)
	}
}

func TestRunForwardsTerminationSignal(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ready := dir + "/ready"
	marker := dir + "/signal"
	t.Setenv("CODEX_PROVIDER_SWITCHER_TEST_HELPER", "signal")
	t.Setenv("CODEX_PROVIDER_SWITCHER_TEST_READY", ready)
	t.Setenv("CODEX_PROVIDER_SWITCHER_TEST_MARKER", marker)

	signals := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(Options{
			Config:  config.Config{Provider: "provider-a", Socket: "/tmp/app-server.sock", Codex: executable},
			Stdin:   strings.NewReader(""),
			Stdout:  io.Discard,
			Stderr:  io.Discard,
			Signals: signals,
		})
	}()

	waitForFile(t, ready)
	signals <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after forwarded signal")
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "terminated" {
		t.Errorf("signal marker = %q", data)
	}
}

func TestRunKillsAndReapsChildAfterRewriteError(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := t.TempDir() + "/pid"
	t.Setenv("CODEX_PROVIDER_SWITCHER_TEST_HELPER", "linger")
	t.Setenv("CODEX_PROVIDER_SWITCHER_TEST_PID", pidFile)

	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Run(Options{
			Config: config.Config{Provider: "provider-a", Socket: "/tmp/app-server.sock", Codex: executable},
			Stdin:  reader,
			Stdout: io.Discard,
			Stderr: io.Discard,
		})
	}()
	waitForFile(t, pidFile)
	_, _ = io.WriteString(writer, "not-json-secret\n")
	_ = writer.Close()
	err = <-done
	if err == nil || !strings.Contains(err.Error(), "input forwarding") {
		t.Fatalf("Run() error = %v, want input forwarding failure", err)
	}
	if strings.Contains(err.Error(), "not-json-secret") {
		t.Fatalf("Run() leaked input body: %v", err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child process %d still exists: %v", pid, err)
	}
}

func TestRunTerminatesChildAfterOutputFailure(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_PROVIDER_SWITCHER_TEST_HELPER", "echo")

	err = Run(Options{
		Config: config.Config{Provider: "provider-a", Socket: "/tmp/app-server.sock", Codex: executable},
		Stdin:  strings.NewReader(`{"method":"custom/do"}` + "\n"),
		Stdout: errorWriter{},
		Stderr: io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "output forwarding") {
		t.Fatalf("Run() error = %v, want output forwarding failure", err)
	}
}

func TestRunReportsStartupFailure(t *testing.T) {
	err := Run(Options{
		Config: config.Config{Provider: "provider-a", Socket: "/tmp/app-server.sock", Codex: "/does/not/exist/codex"},
		Stdin:  strings.NewReader(""),
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "start Codex proxy") {
		t.Fatalf("Run() error = %v, want startup failure", err)
	}
}

func runHelper(mode string) {
	switch mode {
	case "echo":
		_, _ = fmt.Fprintln(os.Stderr, strings.Join(os.Args[1:], " "))
		_, _ = io.Copy(os.Stdout, os.Stdin)
	case "exit":
		code, _ := strconv.Atoi(os.Getenv("CODEX_PROVIDER_SWITCHER_TEST_EXIT"))
		os.Exit(code)
	case "signal":
		_ = os.WriteFile(os.Getenv("CODEX_PROVIDER_SWITCHER_TEST_READY"), []byte("ready"), 0o600)
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		<-signals
		_ = os.WriteFile(os.Getenv("CODEX_PROVIDER_SWITCHER_TEST_MARKER"), []byte("terminated"), 0o600)
	case "linger":
		_ = os.WriteFile(os.Getenv("CODEX_PROVIDER_SWITCHER_TEST_PID"), []byte(strconv.Itoa(os.Getpid())), 0o600)
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGUSR1)
		<-signals
	default:
		os.Exit(99)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) {
	return 0, errors.New("test output failure")
}
