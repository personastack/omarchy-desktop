package subprocess

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

const waitDelay = 250 * time.Millisecond

// Run bounds both command lifetime and inherited output-pipe lifetime. It owns
// the command's process group so cancellation also stops command descendants.
func Run(ctx context.Context, command *exec.Cmd) error {
	if ctx == nil {
		return errors.New("command context is required")
	}
	if command == nil {
		return errors.New("command is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = waitDelay
	if err := command.Start(); err != nil {
		return fmt.Errorf("start owned command: %w", err)
	}
	processGroupID := command.Process.Pid
	cancellationFinished := make(chan struct{})
	var cancellationErr error
	stopCancellation := context.AfterFunc(ctx, func() {
		defer close(cancellationFinished)
		cancellationErr = terminateGroup(processGroupID)
	})
	err := command.Wait()
	if !stopCancellation() {
		<-cancellationFinished
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		if cancellationErr != nil {
			return fmt.Errorf("terminate owned command after cancellation: %w", cancellationErr)
		}
		return ctxErr
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		if killErr := terminateGroup(processGroupID); killErr != nil {
			return fmt.Errorf("terminate owned command after output timeout: %w", killErr)
		}
	}
	if err != nil {
		return fmt.Errorf("wait for owned command: %w", err)
	}
	return nil
}

func terminateGroup(processGroupID int) error {
	err := syscall.Kill(-processGroupID, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
