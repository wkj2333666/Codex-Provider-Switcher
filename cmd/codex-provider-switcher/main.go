package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/wkj2333666/Codex-Provider-Switcher/internal/config"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/proxy"
)

var version = "dev"

const usage = `Usage: codex-provider-switcher [options]

Options:
  --provider <id>  Provider to inject into task requests
  --socket <path>  Shared app-server Unix socket
  --codex <path>   Codex executable or command name; defaults to codex
  --version        Print the switcher version and exit
  --help           Print this help and exit

Environment:
  CODEX_PROVIDER_SWITCHER_PROVIDER
  CODEX_PROVIDER_SWITCHER_SOCKET
  CODEX_PROVIDER_SWITCHER_CODEX
`

type dependencies struct {
	getenv   func(string) string
	stdin    io.Reader
	stdout   io.Writer
	stderr   io.Writer
	signals  <-chan os.Signal
	runProxy func(proxy.Options) error
}

func main() {
	os.Exit(realMain())
}

func realMain() int {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)

	return run(os.Args[1:], dependencies{
		getenv:   os.Getenv,
		stdin:    os.Stdin,
		stdout:   os.Stdout,
		stderr:   os.Stderr,
		signals:  signals,
		runProxy: proxy.Run,
	})
}

func run(args []string, deps dependencies) int {
	result, err := config.Parse(args, deps.getenv)
	if errors.Is(err, flag.ErrHelp) {
		_, _ = fmt.Fprint(deps.stdout, usage)
		return 0
	}
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "codex-provider-switcher: configuration error: %v\n", err)
		return 2
	}
	if result.ShowVersion {
		_, _ = fmt.Fprintf(deps.stdout, "codex-provider-switcher %s\n", version)
		return 0
	}

	err = deps.runProxy(proxy.Options{
		Config:  result.Config,
		Stdin:   deps.stdin,
		Stdout:  deps.stdout,
		Stderr:  deps.stderr,
		Signals: deps.signals,
	})
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "codex-provider-switcher: proxy error: %v\n", err)
	}
	return proxy.ExitCode(err)
}
