package observability

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// HealthStatus represents the overall health of the application.
type HealthStatus struct {
	Status    string            `json:"status"`
	Modules   map[string]string `json:"modules"`
	Uptime    string            `json:"uptime"`
	Timestamp string            `json:"timestamp"`
}

// HealthChecker provides health check reporting for all modules.
type HealthChecker struct {
	mu      sync.RWMutex
	modules map[string]string
	started time.Time
	status  string
}

// NewHealthChecker creates a health checker with all modules initially healthy.
func NewHealthChecker() *HealthChecker {
	return &HealthChecker{
		modules: make(map[string]string),
		started: time.Now(),
		status:  "healthy",
	}
}

// SetModuleStatus records the health status of a module.
func (h *HealthChecker) SetModuleStatus(name, status string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.modules[name] = status
	if status == "unhealthy" {
		if h.status != "unhealthy" {
			h.status = "degraded"
		}
	} else {
		// Check if any module is unhealthy
		anyUnhealthy := false
		for _, s := range h.modules {
			if s == "unhealthy" {
				anyUnhealthy = true
				break
			}
		}
		if anyUnhealthy {
			h.status = "degraded"
		} else if len(h.modules) == 0 {
			h.status = "healthy"
		}
	}
}

// UnsetModuleStatus removes a module from health tracking.
func (h *HealthChecker) UnsetModuleStatus(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.modules, name)
}

// ModuleStatus returns the current status of a specific module.
func (h *HealthChecker) ModuleStatus(name string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.modules[name]
}

// Status returns the current health status of the application.
func (h *HealthChecker) Status() HealthStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()

	modules := make(map[string]string, len(h.modules))
	for k, v := range h.modules {
		modules[k] = v
	}

	return HealthStatus{
		Status:    h.status,
		Modules:   modules,
		Uptime:    time.Since(h.started).Round(time.Second).String(),
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}
}

// Handler returns an HTTP handler for the health check endpoint.
func (h *HealthChecker) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s := h.Status()
		w.Header().Set("Content-Type", "application/json")
		if s.Status == "unhealthy" {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		json.NewEncoder(w).Encode(s)
	}
}
