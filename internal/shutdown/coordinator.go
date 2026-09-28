// Package shutdown implements a deterministic, multi-phase graceful shutdown
// coordinator for MediaCruncher.
//
// Architecture:
//
//	The Coordinator enforces a fixed 4-phase shutdown sequence when triggered
//	(by OS signal via ListenSignals() or explicit Execute() call):
//
//	  Phase 1 "Draining Workers" — stop filesystem scanner, drain worker pool
//	                    (timeout: configurable, default 5m)
//	  Phase 2 "State Persistence" — stop notification engine, flush queues
//	              (timeout: 30s)
//	  Phase 3 "Connection Teardown" — shutdown HTTP server, close DB connections
//	               (timeout: 15s)
//	  Phase 4 "Final Reporting" — write final metrics, log shutdown complete
//	                (timeout: 5s)
//
// Each phase runs its handlers concurrently (sync.WaitGroup) and waits for
// either completion or timeout. Failed handlers are logged but do not block
// progression to the next phase.
//
// Force-kill watchdog:
//	After all phases complete, a goroutine watches for forceKillTimeout. If the
//	process hasn't exited naturally (e.g., stuck container signal handler), it
//	calls os.Exit(1) to guarantee termination. This is critical for container
//	environments where docker stop may have a default 10s limit.
//
// Idempotency:
//	Execute() is safe to call multiple times — the first call wins and subsequent
//	calls block on the shutdownDone channel until completion.
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
	forceKillTimeout    time.Duration // additional timeout after graceful shutdown before force exit
}

func NewCoordinator(drainTimeout time.Duration) *Coordinator {
	return NewCoordinatorWithForceKill(drainTimeout, 30*time.Second)
}

// NewCoordinatorWithForceKill creates a Coordinator with a force-kill fallback.
// After drainTimeout expires during graceful shutdown, forceKillTimeout is used
// to ensure the process exits even if some handlers hang.
func NewCoordinatorWithForceKill(drainTimeout, forceKillTimeout time.Duration) *Coordinator {
	if drainTimeout <= 0 {
		drainTimeout = 5 * time.Minute
	}
	if forceKillTimeout <= 0 {
		forceKillTimeout = 30 * time.Second
	}
	return &Coordinator{
		shutdownDone:     make(chan struct{}),
		drainTimeout:     drainTimeout,
		forceKillTimeout: forceKillTimeout,
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

// Execute executes the shutdown sequence with a force-kill fallback.
// It is idempotent. After all phases complete or the drain timeout is reached,
// if forceKillTimeout was configured, a final watchdog ensures process exit.
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

	// Force-kill watchdog: if we're running in a container and something is
	// still blocking exit, ensure we terminate after forceKillTimeout.
	if c.forceKillTimeout > 0 {
		go func() {
			select {
			case <-time.After(c.forceKillTimeout):
				slog.Error("shutdown: force-kill timeout reached, exiting forcefully")
				os.Exit(1)
			case <-c.shutdownDone:
				// Already exited normally
			}
		}()
	}
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
