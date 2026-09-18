package proc

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCircularBuffer(t *testing.T) {
	cb := NewCircularBuffer(10)
	cb.Write([]byte("hello"))
	if string(cb.Bytes()) != "hello" {
		t.Fatalf("expected 'hello', got '%s'", string(cb.Bytes()))
	}
	cb.Write([]byte(" world!")) // total length exceeds 10
	out := string(cb.Bytes())
	if len(out) != 10 {
		t.Fatalf("expected length 10, got %d", len(out))
	}
	if !strings.HasSuffix(out, "world!") {
		t.Fatalf("expected suffix 'world!', got '%s'", out)
	}
}

func TestRunCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stdout, stderr, err := RunCommand(ctx, "go", "version")
	if err != nil {
		t.Fatalf("expected no error, got %v (stderr: %s)", err, string(stderr))
	}
	if !strings.Contains(string(stdout), "go version") {
		t.Fatalf("expected 'go version' in output, got: %s", string(stdout))
	}
}
