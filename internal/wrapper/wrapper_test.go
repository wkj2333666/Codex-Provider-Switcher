package wrapper

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestClassifyInterceptsOnlyAppServerProxy(t *testing.T) {
	action, proxyArgs, err := Classify([]string{"app-server", "proxy", "--sock", "/tmp/a.sock"}, env(nil))
	if err != nil {
		t.Fatalf("Classify() error = %v", err)
	}
	if action != Proxy || !slices.Equal(proxyArgs, []string{"--socket", "/tmp/a.sock"}) {
		t.Fatalf("Classify() = %v, %q", action, proxyArgs)
	}

	action, proxyArgs, err = Classify([]string{"app-server", "proxy", "--sock=/tmp/b.sock"}, env(nil))
	if err != nil {
		t.Fatalf("Classify(--sock=value) error = %v", err)
	}
	if action != Proxy || !slices.Equal(proxyArgs, []string{"--socket", "/tmp/b.sock"}) {
		t.Fatalf("Classify(--sock=value) = %v, %q", action, proxyArgs)
	}

	for _, args := range [][]string{
		nil,
		{"--version"},
		{"app-server", "--listen", "stdio://"},
		{"exec", "echo", "hello"},
	} {
		action, proxyArgs, err := Classify(args, env(nil))
		if err != nil || action != Delegate || proxyArgs != nil {
			t.Fatalf("Classify(%q) = %v, %q, %v", args, action, proxyArgs, err)
		}
	}
}

func TestClassifyInterceptsProxyWithCommonOptionsAtEveryLayer(t *testing.T) {
	tests := [][]string{
		{"app-server", "proxy"},
		{"-c", `model="x"`, "app-server", "proxy"},
		{"--config=model=\"x\"", "app-server", "proxy"},
		{"--enable", "feature-a", "app-server", "proxy"},
		{"--disable=feature-b", "app-server", "proxy"},
		{"--strict-config", "app-server", "proxy"},
		{"app-server", "-c", `model="x"`, "proxy"},
		{"app-server", "--enable=feature-a", "proxy"},
		{"app-server", "--disable", "feature-b", "proxy"},
		{"app-server", "--strict-config", "proxy"},
		{"app-server", "proxy", "-c", `model="x"`},
		{"app-server", "proxy", "--config=model=\"x\""},
		{"app-server", "proxy", "--enable", "feature-a"},
		{"app-server", "proxy", "--disable=feature-b"},
		{"app-server", "proxy", "-c", "--help"},
		{
			"--enable", "top-a", "--disable=top-b", "app-server",
			"--config", `model="x"`, "--strict-config", "proxy",
			"-c", `model_reasoning_effort="high"`, "--enable=proxy-a",
		},
	}
	for _, args := range tests {
		action, proxyArgs, err := Classify(args, env(map[string]string{
			"CODEX_HOME": "/tmp/codex-home",
		}))
		if err != nil || action != Proxy {
			t.Fatalf("Classify(%q) = %v, %q, %v", args, action, proxyArgs, err)
		}
		want := "/tmp/codex-home/app-server-control/app-server-control.sock"
		if !slices.Equal(proxyArgs, []string{"--socket", want}) {
			t.Fatalf("Classify(%q) proxy args = %q, want socket %q", args, proxyArgs, want)
		}
	}
}

func TestClassifyFailsClosedForAmbiguousProxyOptions(t *testing.T) {
	tests := [][]string{
		{"--secret-option", "app-server", "proxy"},
		{"app-server", "--secret-option", "proxy"},
		{"app-server", "proxy", "--secret-option"},
		{"app-server", "proxy", "-c"},
		{"app-server", "proxy", "--enable="},
		{"app-server", "proxy", "--strict-config"},
	}
	for _, args := range tests {
		action, _, err := Classify(args, env(map[string]string{"CODEX_HOME": "/tmp/home"}))
		if action != Proxy || err == nil {
			t.Fatalf("Classify(%q) = %v, %v", args, action, err)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatalf("Classify(%q) leaked argument in %v", args, err)
		}
	}
}

func TestClassifyDelegatesKnownNonProxyCommands(t *testing.T) {
	tests := [][]string{
		{"exec", "echo", "app-server", "proxy"},
		{"review", "app-server", "proxy"},
		{"app-server", "daemon", "proxy"},
		{"app-server", "proxy", "--help"},
		{"app-server", "proxy", "-h"},
	}
	for _, args := range tests {
		action, proxyArgs, err := Classify(args, env(nil))
		if err != nil || action != Delegate || proxyArgs != nil {
			t.Fatalf("Classify(%q) = %v, %q, %v", args, action, proxyArgs, err)
		}
	}
}

func TestClassifyRejectsUnsafeProxyArgumentsWithoutDelegating(t *testing.T) {
	tests := [][]string{
		{"app-server", "proxy", "--sock"},
		{"app-server", "proxy", "--sock", ""},
		{"app-server", "proxy", "--unknown"},
		{"app-server", "proxy", "--sock", "/secret/one.sock", "--sock", "/secret/two.sock"},
		{"app-server", "proxy", "secret-positional"},
	}
	for _, args := range tests {
		action, _, err := Classify(args, env(nil))
		if action != Proxy || err == nil {
			t.Fatalf("Classify(%q) = action %v, error %v", args, action, err)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatalf("Classify(%q) leaked argument in %v", args, err)
		}
	}
}

func TestClassifyResolvesWrapperSocketPrecedence(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{
			name: "explicit",
			args: []string{"app-server", "proxy", "--sock", "/explicit.sock"},
			env: map[string]string{
				"CODEX_PROVIDER_SWITCHER_SOCKET": "/switcher-env.sock",
				"CODEX_HOME":                     "/codex-home",
			},
			want: "/explicit.sock",
		},
		{
			name: "switcher environment",
			args: []string{"app-server", "proxy"},
			env: map[string]string{
				"CODEX_PROVIDER_SWITCHER_SOCKET": "/switcher-env.sock",
				"CODEX_HOME":                     "/codex-home",
			},
			want: "/switcher-env.sock",
		},
		{
			name: "Codex home",
			args: []string{"app-server", "proxy"},
			env:  map[string]string{"CODEX_HOME": "/codex-home"},
			want: "/codex-home/app-server-control/app-server-control.sock",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action, proxyArgs, err := Classify(tt.args, env(tt.env))
			if err != nil || action != Proxy {
				t.Fatalf("Classify() = %v, %q, %v", action, proxyArgs, err)
			}
			if !slices.Equal(proxyArgs, []string{"--socket", tt.want}) {
				t.Fatalf("proxy args = %q, want socket %q", proxyArgs, tt.want)
			}
		})
	}
}

func TestClassifyFallsBackToUserCodexHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	action, proxyArgs, err := Classify([]string{"app-server", "proxy"}, env(nil))
	if err != nil || action != Proxy {
		t.Fatalf("Classify() = %v, %q, %v", action, proxyArgs, err)
	}
	want := filepath.Join(home, ".codex", "app-server-control", "app-server-control.sock")
	if !slices.Equal(proxyArgs, []string{"--socket", want}) {
		t.Fatalf("proxy args = %q, want socket %q", proxyArgs, want)
	}
}

func TestClassifyRejectsUnavailableUserHome(t *testing.T) {
	t.Setenv("HOME", "")
	action, _, err := Classify([]string{"app-server", "proxy"}, env(nil))
	if action != Proxy || err == nil {
		t.Fatalf("Classify() = %v, %v", action, err)
	}
	if err.Error() != "resolve default Codex socket" {
		t.Fatalf("Classify() error = %q", err)
	}
}

func TestResolveRealCodexUsesAbsoluteEnvironmentOverride(t *testing.T) {
	current := executable(t, filepath.Join(t.TempDir(), "switcher"))
	realCodex := executable(t, filepath.Join(t.TempDir(), "codex-real"))

	got, err := ResolveRealCodex(current, env(map[string]string{
		"CODEX_PROVIDER_SWITCHER_CODEX": realCodex,
		"PATH":                          "",
	}))
	if err != nil {
		t.Fatalf("ResolveRealCodex() error = %v", err)
	}
	if got != realCodex {
		t.Fatalf("ResolveRealCodex() = %q, want %q", got, realCodex)
	}
}

func TestResolveRealCodexSearchesPastRecursiveLinks(t *testing.T) {
	current := executable(t, filepath.Join(t.TempDir(), "switcher"))
	symlinkDir := t.TempDir()
	if err := os.Symlink(current, filepath.Join(symlinkDir, "codex")); err != nil {
		t.Fatal(err)
	}
	hardlinkDir := t.TempDir()
	if err := os.Link(current, filepath.Join(hardlinkDir, "codex")); err != nil {
		t.Fatal(err)
	}
	realDir := t.TempDir()
	realCodex := executable(t, filepath.Join(realDir, "codex"))

	got, err := ResolveRealCodex(current, env(map[string]string{
		"PATH": strings.Join([]string{symlinkDir, hardlinkDir, realDir}, string(os.PathListSeparator)),
	}))
	if err != nil {
		t.Fatalf("ResolveRealCodex() error = %v", err)
	}
	if got != realCodex {
		t.Fatalf("ResolveRealCodex() = %q, want %q", got, realCodex)
	}
}

func TestResolveRealCodexRejectsUnsafeOrRecursiveCandidates(t *testing.T) {
	current := executable(t, filepath.Join(t.TempDir(), "switcher"))
	recursiveDir := t.TempDir()
	recursivePath := filepath.Join(recursiveDir, "codex")
	if err := os.Symlink(current, recursivePath); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "relative override", env: map[string]string{"CODEX_PROVIDER_SWITCHER_CODEX": "secret-relative-codex"}},
		{name: "recursive override", env: map[string]string{"CODEX_PROVIDER_SWITCHER_CODEX": recursivePath}},
		{name: "recursive PATH only", env: map[string]string{"PATH": recursiveDir}},
		{name: "missing PATH", env: map[string]string{"PATH": filepath.Join(t.TempDir(), "secret-missing")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ResolveRealCodex(current, env(tt.env))
			if err == nil {
				t.Fatal("ResolveRealCodex() error = nil")
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), recursivePath) {
				t.Fatalf("ResolveRealCodex() leaked candidate: %v", err)
			}
		})
	}
}

func executable(t *testing.T, path string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}
