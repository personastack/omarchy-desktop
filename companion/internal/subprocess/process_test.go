package subprocess

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"testing"
)

func TestRunStopsCommandDescendantsOnCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := exec.Command("/bin/sh", "-c", "(sleep 30) & printf ready")
	command.Stdout = cancelOnWrite{cancel: cancel}
	command.Stderr = io.Discard
	if err := Run(ctx, command); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context cancellation", err)
	}
}

func TestRunStopsDescendantsWhenTheyKeepOutputOpen(t *testing.T) {
	t.Parallel()
	command := exec.Command("/bin/sh", "-c", "(sleep 30) & printf ready")
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := Run(context.Background(), command); !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("Run() error = %v, want output wait timeout", err)
	}
}

type cancelOnWrite struct {
	cancel context.CancelFunc
}

func (writer cancelOnWrite) Write(data []byte) (int, error) {
	writer.cancel()
	return len(data), nil
}
