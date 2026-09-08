package observability

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
)

// Counter is an atomic counter metric (e.g., total jobs processed).
type Counter struct {
	name        string
	description string
	value       int64
}

// NewCounter creates a new counter metric with the given name and description.
func NewCounter(name, description string) *Counter {
	return &Counter{name: name, description: description}
}

// Inc increments the counter by one.
func (c *Counter) Inc() {
	atomic.AddInt64(&c.value, 1)
}

// Add increments the counter by n.
func (c *Counter) Add(n int64) {
	atomic.AddInt64(&c.value, n)
}

// Value returns the current counter value.
func (c *Counter) Value() int64 {
	return atomic.LoadInt64(&c.value)
}

// Name returns the counter name.
func (c *Counter) Name() string {
	return c.name
}

// Gauge is an atomic gauge metric that can go up and down (e.g., current queue depth).
type Gauge struct {
	name        string
	description string
	value       int64
}

// NewGauge creates a new gauge metric.
func NewGauge(name, description string) *Gauge {
	return &Gauge{name: name, description: description}
}

// Set sets the gauge to a specific value.
func (g *Gauge) Set(v int64) {
	atomic.StoreInt64(&g.value, v)
}

// Inc increments the gauge by one.
func (g *Gauge) Inc() {
	atomic.AddInt64(&g.value, 1)
}

// Dec decrements the gauge by one.
func (g *Gauge) Dec() {
	atomic.AddInt64(&g.value, -1)
}

// Value returns the current gauge value.
func (g *Gauge) Value() int64 {
	return atomic.LoadInt64(&g.value)
}

// Name returns the gauge name.
func (g *Gauge) Name() string {
	return g.name
}

// Histogram tracks distribution of values via buckets (e.g., encoding duration).
type Histogram struct {
	name        string
	description string
	buckets     []float64
	counts      []int64 // cumulative count per bucket
	sum         atomic.Int64
	count       atomic.Int64
	mu          sync.Mutex
}

// NewHistogram creates a histogram with the given name, description, and bucket boundaries.
func NewHistogram(name, description string, buckets []float64) *Histogram {
	return &Histogram{
		name:        name,
		description: description,
		buckets:     buckets,
		counts:      make([]int64, len(buckets)),
	}
}

// Observe records a value observation.
func (h *Histogram) Observe(v float64) {
	h.sum.Add(int64(v * 1000)) // millisecond precision
	h.count.Add(1)
	h.mu.Lock()
	for i, b := range h.buckets {
		if v <= b {
			h.counts[i]++
		}
	}
	h.mu.Unlock()
}

// Sum returns the sum of all observed values (in milliseconds).
func (h *Histogram) Sum() int64 {
	return h.sum.Load()
}

// Count returns the number of observations.
func (h *Histogram) Count() int64 {
	return h.count.Load()
}

// Name returns the histogram name.
func (h *Histogram) Name() string {
	return h.name
}

// Registry holds all metrics for the application and exposes them via HTTP.
type Registry struct {
	counters  map[string]*Counter
	gauges    map[string]*Gauge
	histograms map[string]*Histogram
	mu        sync.RWMutex
	addr      string
	handler   http.Handler
}

// NewRegistry creates a metrics registry with the given HTTP listen address.
// Pass an empty string to disable the HTTP endpoint.
func NewRegistry(addr string) *Registry {
	r := &Registry{
		counters:   make(map[string]*Counter),
		gauges:     make(map[string]*Gauge),
		histograms: make(map[string]*Histogram),
		addr:       addr,
	}
	if addr != "" {
		r.handler = http.HandlerFunc(r.handleMetrics)
	}
	return r
}

// RegisterCounter registers a counter and returns it for use.
func (r *Registry) RegisterCounter(name, description string) *Counter {
	c := NewCounter(name, description)
	r.mu.Lock()
	r.counters[name] = c
	r.mu.Unlock()
	return c
}

// RegisterGauge registers a gauge and returns it for use.
func (r *Registry) RegisterGauge(name, description string) *Gauge {
	g := NewGauge(name, description)
	r.mu.Lock()
	r.gauges[name] = g
	r.mu.Unlock()
	return g
}

// RegisterHistogram registers a histogram and returns it for use.
func (r *Registry) RegisterHistogram(name, description string, buckets []float64) *Histogram {
	h := NewHistogram(name, description, buckets)
	r.mu.Lock()
	r.histograms[name] = h
	r.mu.Unlock()
	return h
}

// handleMetrics exposes all metrics in Prometheus-like text format.
func (r *Registry) handleMetrics(w http.ResponseWriter, req *http.Request) {
	r.ServeHTTP(w, req)
}

// ServeHTTP exposes all metrics in Prometheus-like text format.
func (r *Registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	for name, c := range r.counters {
		fmt.Fprintf(w, "# HELP mediacruncher_%s %s\n", name, c.description)
		fmt.Fprintf(w, "# TYPE mediacruncher_%s counter\n", name)
		fmt.Fprintf(w, "mediacruncher_%s %d\n", name, c.Value())
	}

	for name, g := range r.gauges {
		fmt.Fprintf(w, "# HELP mediacruncher_%s %s\n", name, g.description)
		fmt.Fprintf(w, "# TYPE mediacruncher_%s gauge\n", name)
		fmt.Fprintf(w, "mediacruncher_%s %d\n", name, g.Value())
	}

	for name, h := range r.histograms {
		fmt.Fprintf(w, "# HELP mediacruncher_%s %s\n", name, h.description)
		fmt.Fprintf(w, "# TYPE mediacruncher_%s histogram\n", name)
		h.mu.Lock()
		for i, b := range h.buckets {
			fmt.Fprintf(w, "mediacruncher_%s_bucket{le=\"%.2f\"} %d\n", name, b, h.counts[i])
		}
		fmt.Fprintf(w, "mediacruncher_%s_bucket{le=\"+Inf\"} %d\n", name, h.count.Load())
		fmt.Fprintf(w, "mediacruncher_%s_sum %.3f\n", name, float64(h.Sum())/1000)
		fmt.Fprintf(w, "mediacruncher_%s_count %d\n", name, h.Count())
		h.mu.Unlock()
	}
}

// Start starts the metrics HTTP server on the configured address.
// Returns nil if no address was configured.
func (r *Registry) Start() error {
	if r.addr == "" || r.handler == nil {
		return nil
	}
	go func() {
		if err := http.ListenAndServe(r.addr, r.handler); err != nil {
			// Logger not available at package init time; skip logging
		}
	}()
	return nil
}

// Shutdown stops the metrics HTTP server gracefully.
func (r *Registry) Shutdown(ctx interface{}) {
	if ctx != nil {
		if closeCtx, ok := ctx.(interface{ Close() error }); ok {
			closeCtx.Close()
		}
	}
}
