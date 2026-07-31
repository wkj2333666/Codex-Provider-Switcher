// Package proxy owns the Codex stdio proxy child process and its data paths.
package proxy

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/wkj2333666/Codex-Provider-Switcher/internal/config"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/rewrite"
)

// Options supplies validated configuration and the process-facing streams.
type Options struct {
	Config  config.Config
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Signals <-chan os.Signal
}

// Run starts the official Codex stdio proxy, rewrites client input, and copies
// server output without inspecting it.
func Run(options Options) error {
	stdin := options.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	stdout := options.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := options.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	cmd := exec.Command(options.Config.Codex, "app-server", "proxy", "--sock", options.Config.Socket)
	cmd.Stderr = stderr
	childInput, err := cmd.StdinPipe()
	if err != nil {
		return errors.New("create Codex proxy input")
	}
	childOutput, err := cmd.StdoutPipe()
	if err != nil {
		return errors.New("create Codex proxy output")
	}
	if err := cmd.Start(); err != nil {
		return sanitizedStartError(err)
	}

	inputDone := make(chan error, 1)
	go func() {
		forwardInput(inputDone, childInput, stdin, options.Config.Provider)
	}()

	outputDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(stdout, childOutput)
		outputDone <- copyErr
	}()

	var forwardingErr error
	signals := options.Signals
	outputFinished := false
	for !outputFinished {
		select {
		case inputErr := <-inputDone:
			inputDone = nil
			if inputErr != nil && forwardingErr == nil {
				forwardingErr = fmt.Errorf("input forwarding failed: %w", inputErr)
				kill(cmd.Process)
			}
		case outputErr := <-outputDone:
			outputFinished = true
			if outputErr != nil && forwardingErr == nil {
				forwardingErr = fmt.Errorf("output forwarding failed: %w", outputErr)
				kill(cmd.Process)
			}
		case sig, ok := <-signals:
			if !ok {
				signals = nil
				continue
			}
			if sig == nil {
				continue
			}
			if signalErr := cmd.Process.Signal(sig); signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) && forwardingErr == nil {
				forwardingErr = fmt.Errorf("forward signal: %w", signalErr)
				kill(cmd.Process)
			}
		}
	}

	waitErr := cmd.Wait()
	_ = childInput.Close()
	if inputDone != nil {
		select {
		case inputErr := <-inputDone:
			if inputErr != nil && forwardingErr == nil {
				forwardingErr = fmt.Errorf("input forwarding failed: %w", inputErr)
			}
		default:
		}
	}
	if forwardingErr != nil {
		return forwardingErr
	}
	if waitErr != nil {
		return waitErr
	}
	return nil
}

func forwardInput(done chan<- error, childInput io.WriteCloser, stdin io.Reader, provider string) {
	streamErr := rewrite.Stream(childInput, stdin, provider)
	if streamErr != nil {
		// Publish failures before EOF can make the child exit and close stdout.
		done <- streamErr
		_ = childInput.Close()
		return
	}
	done <- childInput.Close()
}

func sanitizedStartError(err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return errors.New("start Codex proxy: executable unavailable")
	case errors.Is(err, os.ErrPermission):
		return errors.New("start Codex proxy: permission denied")
	default:
		return errors.New("start Codex proxy: failed")
	}
}

func kill(process *os.Process) {
	if process == nil {
		return
	}
	if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return
	}
}

// ExitCode maps normal child status codes directly and all other failures to 1.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() >= 0 {
		return exitError.ExitCode()
	}
	return 1
}
