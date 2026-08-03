// Package config resolves and validates switcher command-line configuration.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"

	providerid "github.com/wkj2333666/Codex-Provider-Switcher/internal/provider"
)

const (
	socketEnvironment   = "CODEX_PROVIDER_SWITCHER_SOCKET"
	stateEnvironment    = "CODEX_PROVIDER_SWITCHER_STATE_DIR"
	recoveryEnvironment = "CODEX_PROVIDER_SWITCHER_RECOVERY"
)

// Config contains fully resolved values for one proxied connection. Provider
// is empty when app-server should apply its own effective configuration.
type Config struct {
	Provider          string
	Socket            string
	StateDir          string
	ExclusiveRecovery bool
}

// Result contains either a runnable configuration or a control action.
type Result struct {
	Config      Config
	ShowVersion bool
}

// ParseProxy resolves direct proxy flags and non-provider environment values,
// then validates the result.
func ParseProxy(args []string, getenv func(string) string) (Result, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	provider := ""
	socket := getenv(socketEnvironment)
	stateDirectory := getenv(stateEnvironment)

	flags := flag.NewFlagSet("codex-provider-switcher", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&provider, "provider", provider, "provider to inject into task requests")
	flags.StringVar(&socket, "socket", socket, "shared app-server Unix socket")
	flags.StringVar(&stateDirectory, "state-dir", stateDirectory, "persistent per-task provider state")
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

	if provider != "" && !providerid.Valid(provider) {
		return Result{}, errors.New("invalid provider: expected only letters, digits, dot, underscore, or hyphen")
	}
	if socket == "" {
		return Result{}, errors.New("socket is required")
	}

	resolvedSocket, err := resolveSocket(socket)
	if err != nil {
		return Result{}, err
	}
	resolvedStateDirectory, err := resolveStateDirectory(stateDirectory, resolvedSocket, getenv)
	if err != nil {
		return Result{}, err
	}

	return Result{Config: Config{
		Provider:          provider,
		Socket:            resolvedSocket,
		StateDir:          resolvedStateDirectory,
		ExclusiveRecovery: getenv(recoveryEnvironment) == "exclusive",
	}}, nil
}

func resolveStateDirectory(directory, socket string, getenv func(string) string) (string, error) {
	if directory != "" {
		absolute, err := filepath.Abs(directory)
		if err != nil {
			return "", errors.New("resolve provider state directory")
		}
		return absolute, nil
	}
	if codexHome := getenv("CODEX_HOME"); codexHome != "" {
		absolute, err := filepath.Abs(filepath.Join(codexHome, "codex-provider-switcher"))
		if err != nil {
			return "", errors.New("resolve provider state directory")
		}
		return absolute, nil
	}
	controlDirectory := filepath.Dir(socket)
	if filepath.Base(socket) == "app-server-control.sock" && filepath.Base(controlDirectory) == "app-server-control" {
		return filepath.Join(filepath.Dir(controlDirectory), "codex-provider-switcher"), nil
	}
	digest := sha256.Sum256([]byte(socket))
	name := ".codex-provider-switcher-" + hex.EncodeToString(digest[:6])
	return filepath.Join(filepath.Dir(socket), name), nil
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
