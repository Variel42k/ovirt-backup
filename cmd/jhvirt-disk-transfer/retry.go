package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var deliveryRetryDelays = []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute}

type transportFailure struct{ error }

func (failure transportFailure) Unwrap() error { return failure.error }

type diagnosticTail struct{ data []byte }

func (tail *diagnosticTail) Write(data []byte) (int, error) {
	const limit = 8192
	n := len(data)
	if n >= limit {
		tail.data = append(tail.data[:0], data[n-limit:]...)
		return n, nil
	}
	if excess := len(tail.data) + n - limit; excess > 0 {
		tail.data = tail.data[:copy(tail.data, tail.data[excess:])]
	}
	tail.data = append(tail.data, data...)
	return n, nil
}

func (tail diagnosticTail) String() string { return string(tail.data) }

type synchronizedOutput struct {
	mu sync.Mutex
	io.Writer
}

func (output *synchronizedOutput) Write(data []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.Writer.Write(data)
}

func captureCommandErrors(command *exec.Cmd, logs io.Writer) *diagnosticTail {
	tail := &diagnosticTail{}
	if _, isTerminalFile := logs.(*os.File); !isTerminalFile {
		logs = &synchronizedOutput{Writer: logs}
	}
	command.Stdout, command.Stderr = logs, io.MultiWriter(logs, tail)
	command.Env = append(command.Environ(), "LC_ALL=C")
	return tail
}

func (dest sshDestination) lostPasswordSession(ctx context.Context, err error) bool {
	var exit *exec.ExitError
	return dest.peer.socket != "" && errors.As(err, &exit) &&
		(exit.ExitCode() == 255 || exit.ExitCode() == 12) && !dest.peer.connected(ctx, dest.host)
}

func transferFailure(err error, stderr string, ssh bool) error {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return err
	}
	diagnostic := strings.ToLower(stderr)
	for _, permanent := range []string{
		"permission denied", "host key verification failed", "host identification has changed",
		"no matching host key", "no space left on device", "disk quota exceeded",
		"read-only file system", "input/output error", "error in file io", "command not found",
	} {
		if strings.Contains(diagnostic, permanent) {
			return err
		}
	}
	switch exit.ExitCode() {
	case 255:
		return transportFailure{err}
	case 10, 12, 30, 35:
		if !ssh {
			return transportFailure{err}
		}
	}
	return err
}

func retryDelivery(ctx context.Context, logs io.Writer, delays []time.Duration, attempt func() error) error {
	for repeat := 0; ; repeat++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := attempt()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var failure transportFailure
		if err == nil || !errors.As(err, &failure) || repeat >= len(delays) {
			return err
		}
		wait := delays[repeat]
		fmt.Fprintf(logs, "Связь прервалась (%v). Возобновление %d/%d через %s.\n", err, repeat+1, len(delays), wait)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
