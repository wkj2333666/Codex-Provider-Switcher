package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/wkj2333666/Codex-Provider-Switcher/internal/config"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/transport"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/wrapper"
)

var (
	version = "dev"
	commit  = "unknown"
	source  = "unknown"
	builtAt = "unknown"
)

type buildInformation struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Source  string `json:"source"`
	BuiltAt string `json:"builtAt"`
}

const usage = `Usage:
  codex-provider-switcher proxy [options]
  codex-provider-switcher --version
  codex-provider-switcher --build-info

Proxy options:
  --provider <id>  Optional explicit provider override
  --socket <path>  Shared app-server Unix socket
  --version        Print the switcher version and exit
  --build-info     Print machine-readable switcher build information and exit
  --help           Print this help and exit

Environment:
  CODEX_PROVIDER_SWITCHER_SOCKET
  CODEX_PROVIDER_SWITCHER_CODEX

When --provider is omitted, app-server configuration selects the provider.
When installed under the name codex, app-server proxy is intercepted and all
other commands are delegated to the real Codex executable.
`

type dependencies struct {
	getenv      func(string) string
	environ     func() []string
	executable  func() (string, error)
	stdin       io.Reader
	stdout      io.Writer
	stderr      io.Writer
	runProxy    func(context.Context, transport.Options) error
	execProcess func(string, []string, []string) error
}

func main() {
	os.Exit(realMain())
}

func realMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	return run(ctx, os.Args[0], os.Args[1:], dependencies{
		getenv:      os.Getenv,
		environ:     os.Environ,
		executable:  os.Executable,
		stdin:       os.Stdin,
		stdout:      os.Stdout,
		stderr:      os.Stderr,
		runProxy:    transport.Run,
		execProcess: syscall.Exec,
	})
}

func run(ctx context.Context, argv0 string, args []string, deps dependencies) int {
	deps = withDefaults(deps)
	if filepath.Base(argv0) == "codex" {
		return runWrapper(ctx, args, deps)
	}
	return runDirect(ctx, args, deps)
}

func runDirect(ctx context.Context, args []string, deps dependencies) int {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "--help" || args[0] == "-h")) {
		_, _ = fmt.Fprint(deps.stdout, usage)
		return 0
	}
	if len(args) == 1 && args[0] == "--version" {
		printVersion(deps.stdout)
		return 0
	}
	if len(args) == 1 && args[0] == "--build-info" {
		if err := printBuildInformation(deps.stdout); err != nil {
			_, _ = fmt.Fprintln(deps.stderr, "codex-provider-switcher: output error: encode build information")
			return 1
		}
		return 0
	}
	if args[0] != "proxy" {
		_, _ = fmt.Fprintln(deps.stderr, "codex-provider-switcher: configuration error: expected proxy subcommand")
		return 2
	}
	return runProxyCommand(ctx, args[1:], deps)
}

func runWrapper(ctx context.Context, args []string, deps dependencies) int {
	action, proxyArgs, err := wrapper.Classify(args, deps.getenv)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "codex-provider-switcher: wrapper error: %v\n", err)
		return 2
	}
	if action == wrapper.Proxy {
		return runProxyCommand(ctx, proxyArgs, deps)
	}

	current, err := deps.executable()
	if err != nil {
		_, _ = fmt.Fprintln(deps.stderr, "codex-provider-switcher: delegation error: inspect wrapper executable")
		return 1
	}
	realCodex, err := wrapper.ResolveRealCodex(current, deps.getenv)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "codex-provider-switcher: delegation error: %v\n", err)
		return 1
	}
	argv := append([]string{realCodex}, args...)
	if err := deps.execProcess(realCodex, argv, deps.environ()); err != nil {
		_, _ = fmt.Fprintln(deps.stderr, "codex-provider-switcher: delegation error: execute real Codex")
		return 1
	}
	return 0
}

func runProxyCommand(ctx context.Context, args []string, deps dependencies) int {
	result, err := config.ParseProxy(args, deps.getenv)
	if errors.Is(err, flag.ErrHelp) {
		_, _ = fmt.Fprint(deps.stdout, usage)
		return 0
	}
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "codex-provider-switcher: configuration error: %v\n", err)
		return 2
	}
	if result.ShowVersion {
		printVersion(deps.stdout)
		return 0
	}

	err = deps.runProxy(ctx, transport.Options{
		Config: result.Config,
		Stdin:  deps.stdin,
		Stdout: deps.stdout,
	})
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "codex-provider-switcher: proxy error: %v\n", err)
		return 1
	}
	return 0
}

func printVersion(writer io.Writer) {
	if commit == "unknown" || commit == "" {
		_, _ = fmt.Fprintf(writer, "codex-provider-switcher %s\n", version)
		return
	}
	shortCommit := commit
	if len(shortCommit) > 7 {
		shortCommit = shortCommit[:7]
	}
	_, _ = fmt.Fprintf(writer, "codex-provider-switcher %s (commit %s)\n", version, shortCommit)
}

func printBuildInformation(writer io.Writer) error {
	return json.NewEncoder(writer).Encode(buildInformation{
		Version: version,
		Commit:  commit,
		Source:  source,
		BuiltAt: builtAt,
	})
}

func withDefaults(deps dependencies) dependencies {
	if deps.getenv == nil {
		deps.getenv = os.Getenv
	}
	if deps.environ == nil {
		deps.environ = os.Environ
	}
	if deps.executable == nil {
		deps.executable = os.Executable
	}
	if deps.stdin == nil {
		deps.stdin = os.Stdin
	}
	if deps.stdout == nil {
		deps.stdout = io.Discard
	}
	if deps.stderr == nil {
		deps.stderr = io.Discard
	}
	if deps.runProxy == nil {
		deps.runProxy = transport.Run
	}
	if deps.execProcess == nil {
		deps.execProcess = syscall.Exec
	}
	return deps
}
