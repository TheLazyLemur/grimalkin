package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func commandInput(command string) json.RawMessage {
	input, _ := json.Marshal(map[string]string{"command": command})
	return input
}

func TestACommandReportsStdoutStderrAndExitCode(t *testing.T) {
	// given
	// ... a command that writes to both streams and fails
	input := commandInput("echo out; echo err >&2; exit 3")

	// when
	// ... it runs
	content, err := Shell{}.Run(context.Background(), input)

	// then
	// ... each stream and the exit code are reported
	if err != nil {
		t.Fatal(err)
	}
	want := "out\n[stderr]\nerr\n[exit code 3]"
	if content != want {
		t.Errorf("content = %q, want %q", content, want)
	}
}

func TestCancellingKillsTheCommandAndItsChildren(t *testing.T) {
	// given
	// ... a command whose child would hold the output pipe open for ten seconds
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()

	// when
	// ... it is cancelled while running
	_, err := Shell{}.Run(ctx, commandInput("sleep 10; echo done"))

	// then
	// ... it returns promptly with the cancellation
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("took %v to return", elapsed)
	}
}
