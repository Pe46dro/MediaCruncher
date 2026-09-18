package proc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

// CircularBuffer is a thread-safe bounded buffer that keeps the last N bytes written.
type CircularBuffer struct {
	mu       sync.Mutex
	buf      []byte
	size     int
	capacity int
}

func NewCircularBuffer(capacity int) *CircularBuffer {
	if capacity <= 0 {
		capacity = 64 * 1024 // 64 KB default
	}
	return &CircularBuffer{
		buf:      make([]byte, capacity),
		capacity: capacity,
	}
}

func (c *CircularBuffer) Write(p []byte) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	n = len(p)
	if n >= c.capacity {
		copy(c.buf, p[n-c.capacity:])
		c.size = c.capacity
		return n, nil
	}

	available := c.capacity - c.size
	if n <= available {
		copy(c.buf[c.size:], p)
		c.size += n
	} else {
		overflow := n - available
		copy(c.buf, c.buf[overflow:c.size])
		copy(c.buf[c.size-overflow:], p)
		c.size = c.capacity
	}
	return n, nil
}

func (c *CircularBuffer) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	res := make([]byte, c.size)
	copy(res, c.buf[:c.size])
	return res
}

// ManagedProcess wraps an execution context with OS process group / job object protection.
type ManagedProcess struct {
	Cmd       *exec.Cmd
	StdoutBuf *CircularBuffer
	StderrBuf *CircularBuffer
	cancel    context.CancelFunc
	done      chan struct{}
	err       error
	cleanupOS func()
}

// RunCommand executes a command under process supervision, returning captured stdout and stderr.
func RunCommand(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.Command(name, args...)
	proc := &ManagedProcess{
		Cmd:       cmd,
		StdoutBuf: NewCircularBuffer(1024 * 1024), // 1MB buffer
		StderrBuf: NewCircularBuffer(1024 * 1024),
		cancel:    cancel,
		done:      make(chan struct{}),
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open stderr pipe: %w", err)
	}

	// Apply OS-level isolation (Job Objects on Windows, Process Groups on POSIX)
	prepareCmd(cmd)

	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("failed to start command %s: %w", name, err)
	}

	// Register with OS supervisor once process has started
	cleanupOS := attachProcessSupervisor(cmd.Process.Pid)
	proc.cleanupOS = cleanupOS

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(proc.StdoutBuf, stdoutPipe)
	}()

	go func() {
		defer wg.Done()
		io.Copy(proc.StderrBuf, stderrPipe)
	}()

	// Monitor context cancellation in background
	go func() {
		select {
		case <-ctx.Done():
			killProcessTree(cmd)
		case <-proc.done:
		}
	}()

	cmdErr := cmd.Wait()
	close(proc.done)
	wg.Wait()

	if proc.cleanupOS != nil {
		proc.cleanupOS()
	}

	stdoutBytes := proc.StdoutBuf.Bytes()
	stderrBytes := proc.StderrBuf.Bytes()

	if ctx.Err() != nil {
		return stdoutBytes, stderrBytes, fmt.Errorf("process timed out or canceled: %w", ctx.Err())
	}
	if cmdErr != nil {
		return stdoutBytes, stderrBytes, fmt.Errorf("process failed with error (%w): %s", cmdErr, string(bytes.TrimSpace(stderrBytes)))
	}

	return stdoutBytes, stderrBytes, nil
}

// RunCommandWithProgress executes a command, streaming stderr lines (e.g. ffmpeg progress) to a callback.
func RunCommandWithProgress(ctx context.Context, onStderrLine func(string), name string, args ...string) ([]byte, []byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.Command(name, args...)
	stdoutBuf := NewCircularBuffer(512 * 1024)
	stderrBuf := NewCircularBuffer(512 * 1024)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, err
	}

	prepareCmd(cmd)

	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}

	cleanupOS := attachProcessSupervisor(cmd.Process.Pid)
	defer func() {
		if cleanupOS != nil {
			cleanupOS()
		}
	}()

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			killProcessTree(cmd)
		case <-done:
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(stdoutBuf, stdoutPipe)
	}()

	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		var lineBuf []byte
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				stderrBuf.Write(chunk)
				for _, b := range chunk {
					if b == '\n' || b == '\r' {
						if len(lineBuf) > 0 && onStderrLine != nil {
							onStderrLine(string(lineBuf))
							lineBuf = lineBuf[:0]
						}
					} else {
						lineBuf = append(lineBuf, b)
					}
				}
			}
			if err != nil {
				break
			}
		}
		if len(lineBuf) > 0 && onStderrLine != nil {
			onStderrLine(string(lineBuf))
		}
	}()

	cmdErr := cmd.Wait()
	close(done)
	wg.Wait()

	stdoutBytes := stdoutBuf.Bytes()
	stderrBytes := stderrBuf.Bytes()

	if ctx.Err() != nil {
		return stdoutBytes, stderrBytes, fmt.Errorf("process cancelled: %w", ctx.Err())
	}
	if cmdErr != nil {
		return stdoutBytes, stderrBytes, fmt.Errorf("command execution failed (%w): %s", cmdErr, string(bytes.TrimSpace(stderrBytes)))
	}

	return stdoutBytes, stderrBytes, nil
}
