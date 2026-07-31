// Package config resolves and validates switcher command-line configuration.
package config

import (
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	providerEnvironment = "CODEX_PROVIDER_SWITCHER_PROVIDER"
	socketEnvironment   = "CODEX_PROVIDER_SWITCHER_SOCKET"
	codexEnvironment    = "CODEX_PROVIDER_SWITCHER_CODEX"
)

var providerPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Config contains fully resolved values safe to pass to the child runner.
type Config struct {
	Provider string
	Socket   string
	Codex    string
}

// Result contains either a runnable configuration or a control action.
type Result struct {
	Config      Config
	ShowVersion bool
}

// Parse resolves flags over environment values and validates the result.
func Parse(args []string, getenv func(string) string) (Result, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	provider := getenv(providerEnvironment)
	socket := getenv(socketEnvironment)
	codex := getenv(codexEnvironment)
	if codex == "" {
		codex = "codex"
	}

	flags := flag.NewFlagSet("codex-provider-switcher", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&provider, "provider", provider, "provider to inject into task requests")
	flags.StringVar(&socket, "socket", socket, "shared app-server Unix socket")
	flags.StringVar(&codex, "codex", codex, "Codex executable or command name")
	showVersion := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(args); err != nil {
		return Result{}, err
	}
	if flags.NArg() != 0 {
		return Result{}, errors.New("unexpected positional argument")
	}
	if *showVersion {
		return Result{ShowVersion: true}, nil
	}

	if provider == "" {
		return Result{}, errors.New("provider is required")
	}
	if !providerPattern.MatchString(provider) {
		return Result{}, errors.New("invalid provider: expected only letters, digits, dot, underscore, or hyphen")
	}
	if socket == "" {
		return Result{}, errors.New("socket is required")
	}

	resolvedSocket, err := resolveSocket(socket)
	if err != nil {
		return Result{}, err
	}
	resolvedCodex, err := resolveExecutable(codex)
	if err != nil {
		return Result{}, err
	}

	return Result{Config: Config{
		Provider: provider,
		Socket:   resolvedSocket,
		Codex:    resolvedCodex,
	}}, nil
}

func resolveSocket(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("resolve socket path")
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", errors.New("inspect socket")
	}
	if info.Mode()&os.ModeSocket == 0 {
		return "", errors.New("socket path is not a Unix socket")
	}
	return absolute, nil
}

func resolveExecutable(value string) (string, error) {
	path := value
	if !strings.ContainsRune(value, os.PathSeparator) {
		resolved, err := exec.LookPath(value)
		if err != nil {
			return "", errors.New("Codex executable not found")
		}
		path = resolved
	}

	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("resolve Codex executable")
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", errors.New("inspect Codex executable")
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("Codex path is not a regular file")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("Codex path is not executable")
	}
	return absolute, nil
}
