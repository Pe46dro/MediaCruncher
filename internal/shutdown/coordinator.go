package shutdown

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

type Phase int

const (
	PhaseDrainingWorkers Phase = iota + 1
	PhaseStatePersistence
	PhaseConnectionTeardown
	PhaseFinalReporting
)

func (p Phase) String() string {
	switch p {
	case PhaseDrainingWorkers:
		return "Draining Workers"
	case PhaseStatePersistence:
		return "State Persistence"
	case PhaseConnectionTeardown:
		return "Connection Teardown"
	case PhaseFinalReporting:
		return "Final Reporting"
	default:
		return "Unknown Phase"
	}
}

// Coordinator orchestrates an ordered graceful shutdown sequence.
type Coordinator struct {
	mu           sync.Mutex
	drainingHandlers    []func(context.Context) error
	persistenceHandlers []func(context.Context) error
	teardownHandlers    []func(context.Context) error
	reportingHandlers   []func(context.Context) error
	shutdownTriggered   bool
	shutdownDone        chan struct{}
	drainTimeout        time.Duration
}

func NewCoordinator(drainTimeout time.Duration) *Coordinator {
	if drainTimeout <= 0 {
		drainTimeout = 5 * time.Minute
	}
	return &Coordinator{
		shutdownDone: make(chan struct{}),
		drainTimeout: drainTimeout,
	}
}

func (c *Coordinator) OnDrainWorkers(fn func(context.Context) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drainingHandlers = append(c.drainingHandlers, fn)
}

func (c *Coordinator) OnPersistState(fn func(context.Context) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.persistenceHandlers = append(c.persistenceHandlers, fn)
}

func (c *Coordinator) OnTeardown(fn func(context.Context) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.teardownHandlers = append(c.teardownHandlers, fn)
}

func (c *Coordinator) OnFinalReporting(fn func(context.Context) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reportingHandlers = append(c.reportingHandlers, fn)
}

// ListenSignals intercepts OS termination signals and initiates the shutdown sequence.
func (c *Coordinator) ListenSignals() {
	sigChan := make(chan os.Signal, 2)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		slog.Warn("shutdown signal intercepted", "signal", sig.String())
		c.Execute()
	}()
}

// Execute executes the 5-phase shutdown sequence. It is idempotent.
func (c *Coordinator) Execute() {
	c.mu.Lock()
	if c.shutdownTriggered {
		c.mu.Unlock()
		<-c.shutdownDone
		return
	}
	c.shutdownTriggered = true
	c.mu.Unlock()

	defer close(c.shutdownDone)

	slog.Info("commencing deterministic graceful shutdown sequence")

	// Phase 1 & 2: Drain Workers & Ingestion
	c.runPhase(PhaseDrainingWorkers, c.drainingHandlers, c.drainTimeout)

	// Phase 3: Flush State Persistence
	c.runPhase(PhaseStatePersistence, c.persistenceHandlers, 30*time.Second)

	// Phase 4: Teardown Connections & Process Handles
	c.runPhase(PhaseConnectionTeardown, c.teardownHandlers, 15*time.Second)

	// Phase 5: Final Metrics & Reporting
	c.runPhase(PhaseFinalReporting, c.reportingHandlers, 5*time.Second)

	slog.Info("graceful shutdown sequence completed successfully")
}

func (c *Coordinator) runPhase(phase Phase, handlers []func(context.Context) error, timeout time.Duration) {
	slog.Info("entering shutdown phase", "phase", phase.String(), "timeout", timeout.String())
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var wg sync.WaitGroup
	for _, handler := range handlers {
		wg.Add(1)
		h := handler
		go func() {
			defer wg.Done()
			if err := h(ctx); err != nil {
				slog.Error("shutdown phase handler failed", "phase", phase.String(), "error", err)
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		slog.Info("completed shutdown phase", "phase", phase.String())
	case <-ctx.Done():
		slog.Warn("shutdown phase timed out", "phase", phase.String(), "error", ctx.Err())
	}
}

// Done returns a channel that is closed when shutdown completes.
func (c *Coordinator) Done() <-chan struct{} {
	return c.shutdownDone
}
