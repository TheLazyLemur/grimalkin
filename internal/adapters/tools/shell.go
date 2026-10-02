package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"

	"grimalkin/internal/core"
)

var shellSpec = core.ToolSpec{
	Name:        "shell",
	Description: "Run a command with /bin/sh -c in the user's working directory and return stdout, stderr and the exit code. Not sandboxed. stdin is closed, so interactive commands will not work.",
	InputSchema: `{"type":"object","properties":{"command":{"type":"string","description":"The shell command to run."}},"required":["command"],"additionalProperties":false}`,
}

type Shell struct{}

func (Shell) Spec() core.ToolSpec { return shellSpec }

func (Shell) Summary(input json.RawMessage) string {
	command, err := parseCommand(input)
	if err != nil {
		return string(input)
	}
	return command
}

func (Shell) Run(ctx context.Context, input json.RawMessage) (string, error) {
	command, err := parseCommand(input)
	if err != nil {
		return "", err
	}
	result, err := run(ctx, command)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("failed to run command: %w", err)
	}
	return result.format(), nil
}

func parseCommand(input json.RawMessage) (string, error) {
	var args struct{ Command *string }
	if json.Unmarshal(input, &args) != nil || args.Command == nil {
		return "", errors.New(`invalid input: expected {"command": string}`)
	}
	return *args.Command, nil
}

type result struct {
	stdout, stderr string
	exitCode       int
}

func run(ctx context.Context, command string) (result, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	// A new session has no controlling terminal, so a command that opens
	// /dev/tty (a TUI, sudo, an ssh prompt) fails instead of hanging. It also
	// makes the command a process group leader, which Cancel relies on.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err := cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return result{}, err
	}
	return result{stdout.String(), stderr.String(), cmd.ProcessState.ExitCode()}, nil
}

func (r result) format() string {
	var formatted strings.Builder
	stdout := strings.TrimRight(r.stdout, "\n")
	stderr := strings.TrimRight(r.stderr, "\n")
	if stdout != "" {
		fmt.Fprintf(&formatted, "%s\n", stdout)
	}
	if stderr != "" {
		fmt.Fprintf(&formatted, "[stderr]\n%s\n", stderr)
	}
	fmt.Fprintf(&formatted, "[exit code %d]", r.exitCode)
	return formatted.String()
}
