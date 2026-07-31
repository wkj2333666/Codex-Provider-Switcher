// Package wrapper classifies transparent Codex wrapper invocations and safely
// resolves the real Codex executable for delegated commands.
package wrapper

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const codexEnvironment = "CODEX_PROVIDER_SWITCHER_CODEX"

// Action describes whether an invocation is delegated or proxied.
type Action uint8

const (
	Delegate Action = iota
	Proxy
)

// Classify intercepts every app-server proxy invocation. Invalid proxy
// arguments return Proxy with an error so callers cannot fall back to Codex.
func Classify(args []string) (Action, []string, error) {
	if len(args) < 2 || args[0] != "app-server" || args[1] != "proxy" {
		return Delegate, nil, nil
	}

	var socket string
	for index := 2; index < len(args); index++ {
		argument := args[index]
		switch {
		case argument == "--sock":
			if socket != "" || index+1 >= len(args) || args[index+1] == "" {
				return Proxy, nil, errors.New("invalid app-server proxy socket option")
			}
			index++
			socket = args[index]
		case strings.HasPrefix(argument, "--sock="):
			if socket != "" {
				return Proxy, nil, errors.New("duplicate app-server proxy socket option")
			}
			socket = strings.TrimPrefix(argument, "--sock=")
			if socket == "" {
				return Proxy, nil, errors.New("invalid app-server proxy socket option")
			}
		default:
			return Proxy, nil, errors.New("unsupported app-server proxy option")
		}
	}
	if socket == "" {
		return Proxy, nil, errors.New("app-server proxy socket is required")
	}
	return Proxy, []string{"--socket", socket}, nil
}

// ResolveRealCodex returns a non-recursive executable for delegated commands.
func ResolveRealCodex(current string, getenv func(string) string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	currentInfo, err := os.Stat(current)
	if err != nil {
		return "", errors.New("inspect wrapper executable")
	}

	if override := getenv(codexEnvironment); override != "" {
		if !filepath.IsAbs(override) {
			return "", errors.New("real Codex override must be an absolute path")
		}
		return validateCandidate(override, currentInfo)
	}

	for _, directory := range filepath.SplitList(getenv("PATH")) {
		if directory == "" {
			directory = "."
		}
		candidate := filepath.Join(directory, "codex")
		info, statErr := os.Stat(candidate)
		if statErr != nil || !isExecutable(info) || os.SameFile(currentInfo, info) {
			continue
		}
		absolute, absErr := filepath.Abs(candidate)
		if absErr != nil {
			continue
		}
		return absolute, nil
	}

	return "", errors.New("real Codex executable not found")
}

func validateCandidate(path string, currentInfo os.FileInfo) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", errors.New("inspect real Codex executable")
	}
	if !isExecutable(info) {
		return "", errors.New("real Codex path is not executable")
	}
	if os.SameFile(currentInfo, info) {
		return "", errors.New("real Codex executable resolves to wrapper")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("resolve real Codex executable")
	}
	return absolute, nil
}

func isExecutable(info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}
