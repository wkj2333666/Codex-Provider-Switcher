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

const (
	socketEnvironment         = "CODEX_PROVIDER_SWITCHER_SOCKET"
	codexHomeEnvironment      = "CODEX_HOME"
	controlSocketRelativePath = "app-server-control/app-server-control.sock"
)

// Action describes whether an invocation is delegated or proxied.
type Action uint8

const (
	Delegate Action = iota
	Proxy
)

// Classify intercepts every app-server proxy invocation. Invalid proxy
// arguments return Proxy with an error so callers cannot fall back to Codex.
func Classify(args []string, getenv func(string) string) (Action, []string, error) {
	index := 0
	for index < len(args) {
		next, consumed, err := consumeCommonOption(args, index, true)
		if err != nil {
			if looksLikeProxy(args[index:]) {
				return Proxy, nil, err
			}
			return Delegate, nil, nil
		}
		if !consumed {
			break
		}
		index = next
	}
	if index >= len(args) {
		return Delegate, nil, nil
	}
	if args[index] != "app-server" {
		if strings.HasPrefix(args[index], "-") && looksLikeProxy(args[index:]) {
			return Proxy, nil, errors.New("unsupported option before app-server proxy")
		}
		return Delegate, nil, nil
	}

	index++
	for index < len(args) {
		next, consumed, err := consumeCommonOption(args, index, true)
		if err != nil {
			if containsToken(args[index:], "proxy") {
				return Proxy, nil, err
			}
			return Delegate, nil, nil
		}
		if !consumed {
			break
		}
		index = next
	}
	if index >= len(args) {
		return Delegate, nil, nil
	}
	if args[index] != "proxy" {
		if strings.HasPrefix(args[index], "-") && containsToken(args[index:], "proxy") {
			return Proxy, nil, errors.New("unsupported option before app-server proxy")
		}
		return Delegate, nil, nil
	}

	index++
	var socket string
	for index < len(args) {
		if args[index] == "--help" || args[index] == "-h" {
			return Delegate, nil, nil
		}
		next, consumed, err := consumeCommonOption(args, index, false)
		if err != nil {
			return Proxy, nil, err
		}
		if consumed {
			index = next
			continue
		}

		argument := args[index]
		switch {
		case argument == "--sock":
			if socket != "" || index+1 >= len(args) || args[index+1] == "" {
				return Proxy, nil, errors.New("invalid app-server proxy socket option")
			}
			socket = args[index+1]
			index += 2
		case strings.HasPrefix(argument, "--sock="):
			if socket != "" {
				return Proxy, nil, errors.New("duplicate app-server proxy socket option")
			}
			socket = strings.TrimPrefix(argument, "--sock=")
			if socket == "" {
				return Proxy, nil, errors.New("invalid app-server proxy socket option")
			}
			index++
		default:
			return Proxy, nil, errors.New("unsupported app-server proxy option")
		}
	}

	if socket == "" {
		var err error
		socket, err = defaultSocket(getenv)
		if err != nil {
			return Proxy, nil, err
		}
	}
	return Proxy, []string{"--socket", socket}, nil
}

func consumeCommonOption(args []string, index int, allowStrict bool) (int, bool, error) {
	argument := args[index]
	if argument == "--strict-config" {
		if allowStrict {
			return index + 1, true, nil
		}
		return index, false, errors.New("unsupported app-server proxy option")
	}

	for _, option := range []string{"-c", "--config", "--enable", "--disable"} {
		if argument == option {
			if index+1 >= len(args) || args[index+1] == "" {
				return index, false, errors.New("invalid Codex configuration option")
			}
			return index + 2, true, nil
		}
	}

	for _, option := range []string{"--config=", "--enable=", "--disable="} {
		if strings.HasPrefix(argument, option) {
			if strings.TrimPrefix(argument, option) == "" {
				return index, false, errors.New("invalid Codex configuration option")
			}
			return index + 1, true, nil
		}
	}

	return index, false, nil
}

func looksLikeProxy(args []string) bool {
	for index, argument := range args {
		if argument == "app-server" && containsToken(args[index+1:], "proxy") {
			return true
		}
	}
	return false
}

func containsToken(args []string, token string) bool {
	for _, argument := range args {
		if argument == token {
			return true
		}
	}
	return false
}

func defaultSocket(getenv func(string) string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if socket := getenv(socketEnvironment); socket != "" {
		return socket, nil
	}
	if codexHome := getenv(codexHomeEnvironment); codexHome != "" {
		return filepath.Join(codexHome, controlSocketRelativePath), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("resolve default Codex socket")
	}
	return filepath.Join(home, ".codex", controlSocketRelativePath), nil
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
