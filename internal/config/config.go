// Package config resolves and validates switcher command-line configuration.
package config

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

const (
	providerEnvironment = "CODEX_PROVIDER_SWITCHER_PROVIDER"
	socketEnvironment   = "CODEX_PROVIDER_SWITCHER_SOCKET"
)

var providerPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Config contains fully resolved values for one proxied connection.
type Config struct {
	Provider string
	Socket   string
}

// Result contains either a runnable configuration or a control action.
type Result struct {
	Config      Config
	ShowVersion bool
}

// ParseProxy resolves direct proxy flags over environment values and validates
// the result.
func ParseProxy(args []string, getenv func(string) string) (Result, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	provider := getenv(providerEnvironment)
	socket := getenv(socketEnvironment)

	flags := flag.NewFlagSet("codex-provider-switcher", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&provider, "provider", provider, "provider to inject into task requests")
	flags.StringVar(&socket, "socket", socket, "shared app-server Unix socket")
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

	return Result{Config: Config{
		Provider: provider,
		Socket:   resolvedSocket,
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
