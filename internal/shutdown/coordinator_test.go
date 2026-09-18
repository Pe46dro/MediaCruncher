package shutdown

import (
	"context"
	"testing"
	"time"
)

func TestCoordinatorSequence(t *testing.T) {
	c := NewCoordinator(2 * time.Second)

	var sequence []int

	c.OnDrainWorkers(func(ctx context.Context) error {
		sequence = append(sequence, 1)
		return nil
	})
	c.OnPersistState(func(ctx context.Context) error {
		sequence = append(sequence, 2)
		return nil
	})
	c.OnTeardown(func(ctx context.Context) error {
		sequence = append(sequence, 3)
		return nil
	})
	c.OnFinalReporting(func(ctx context.Context) error {
		sequence = append(sequence, 4)
		return nil
	})

	c.Execute()

	if len(sequence) != 4 {
		t.Fatalf("expected 4 handlers called, got %d", len(sequence))
	}
	for i := 0; i < 4; i++ {
		if sequence[i] != i+1 {
			t.Errorf("expected sequence[%d] = %d, got %d", i, i+1, sequence[i])
		}
	}

	// Idempotency: execute again should return immediately
	c.Execute()
}
