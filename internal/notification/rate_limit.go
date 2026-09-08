package notification

import (
	"sync"
	"time"
)

// RateLimitConfig holds rate limit configuration for a channel.
type RateLimitConfig struct {
	MaxPerMinute  int           `json:"max_per_minute"`
	MaxPerHour    int           `json:"max_per_hour"`
	WindowMinutes int           `json:"window_minutes"`
	RetryDelay    time.Duration `json:"retry_delay"`
}

// DefaultRateLimitConfig returns a standard rate limit configuration.
func DefaultRateLimitConfig() *RateLimitConfig {
	return &RateLimitConfig{
		MaxPerMinute:  10,
		MaxPerHour:    100,
		WindowMinutes: 60,
		RetryDelay:    5 * time.Second,
	}
}

// Limiter enforces per-channel rate limits.
type Limiter struct {
	mu       sync.Mutex
	limits   map[string]*ChannelLimiter
	config   *RateLimitConfig
}

// ChannelLimiter tracks rate limit state for a single channel.
type ChannelLimiter struct {
	mu          sync.Mutex
	requests    []time.Time
	maxPerMinute int
	maxPerHour   int
	retryAfter   time.Time
	limitedUntil time.Time
}

// NewLimiter creates a new rate limiter.
func NewLimiter(config *RateLimitConfig) *Limiter {
	if config == nil {
		config = DefaultRateLimitConfig()
	}
	return &Limiter{
		limits: make(map[string]*ChannelLimiter),
		config: config,
	}
}

// Allow checks if a request to a channel is allowed.
func (l *Limiter) Allow(channel string) bool {
	l.mu.Lock()
	limiter, exists := l.limits[channel]
	if !exists {
		limiter = &ChannelLimiter{
			maxPerMinute: l.config.MaxPerMinute,
			maxPerHour:   l.config.MaxPerHour,
		}
		l.limits[channel] = limiter
	}
	l.mu.Unlock()

	return limiter.Allow()
}

// Wait returns the duration to wait before the channel is available.
func (l *Limiter) Wait(channel string) time.Duration {
	l.mu.Lock()
	limiter, exists := l.limits[channel]
	if !exists {
		l.mu.Unlock()
		return 0
	}
	l.mu.Unlock()

	return limiter.WaitTime()
}

// Record records a successful delivery for rate limiting tracking.
func (l *Limiter) Record(channel string) {
	l.mu.Lock()
	limiter, exists := l.limits[channel]
	if !exists {
		limiter = &ChannelLimiter{
			maxPerMinute: l.config.MaxPerMinute,
			maxPerHour:   l.config.MaxPerHour,
		}
		l.limits[channel] = limiter
	}
	l.mu.Unlock()

	limiter.Record()
}

// SetLimit sets custom rate limits for a channel.
func (l *Limiter) SetLimit(channel string, maxPerMinute, maxPerHour int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	limiter, exists := l.limits[channel]
	if !exists {
		limiter = &ChannelLimiter{
			maxPerMinute: maxPerMinute,
			maxPerHour:   maxPerHour,
		}
		l.limits[channel] = limiter
	} else {
		limiter.mu.Lock()
		limiter.maxPerMinute = maxPerMinute
		limiter.maxPerHour = maxPerHour
		limiter.mu.Unlock()
	}
}

// GetStats returns rate limit stats for a channel.
func (l *Limiter) GetStats(channel string) map[string]interface{} {
	l.mu.Lock()
	limiter, exists := l.limits[channel]
	l.mu.Unlock()

	if !exists {
		return map[string]interface{}{
			"channel":     channel,
			"limited":     false,
			"requests_mn": 0,
			"requests_hr": 0,
		}
	}

	return limiter.Stats()
}

// Reset clears all rate limit state.
func (l *Limiter) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limits = make(map[string]*ChannelLimiter)
}

func (cl *ChannelLimiter) Allow() bool {
	cl.mu.Lock()
	defer cl.mu.Unlock()

	now := time.Now()
	if now.Before(cl.limitedUntil) {
		return false
	}

	cl.cleanOldRequests(now)

	minutes := cl.countInWindow(now, 1*time.Minute)
	hours := cl.countInWindow(now, 1*time.Hour)

	if minutes >= cl.maxPerMinute || hours >= cl.maxPerHour {
		cl.limitedUntil = now.Add(1 * time.Minute)
		return false
	}

	return true
}

func (cl *ChannelLimiter) Record() {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.requests = append(cl.requests, time.Now())
}

func (cl *ChannelLimiter) WaitTime() time.Duration {
	cl.mu.Lock()
	defer cl.mu.Unlock()

	if len(cl.requests) == 0 {
		return 0
	}

	now := time.Now()
	minutes := cl.countInWindow(now, 1*time.Minute)

	if minutes >= cl.maxPerMinute {
		return cl.requests[0].Add(1 * time.Minute).Sub(now)
	}

	return 0
}

func (cl *ChannelLimiter) cleanOldRequests(now time.Time) {
	cutoff := now.Add(-2 * time.Hour)
	cleaned := make([]time.Time, 0, len(cl.requests))
	for _, t := range cl.requests {
		if t.After(cutoff) {
			cleaned = append(cleaned, t)
		}
	}
	cl.requests = cleaned
}

func (cl *ChannelLimiter) countInWindow(now time.Time, window time.Duration) int {
	cutoff := now.Add(-window)
	count := 0
	for _, t := range cl.requests {
		if t.After(cutoff) {
			count++
		}
	}
	return count
}

func (cl *ChannelLimiter) Stats() map[string]interface{} {
	cl.mu.Lock()
	defer cl.mu.Unlock()

	now := time.Time{}
	if len(cl.requests) > 0 {
		now = time.Now()
	}

	return map[string]interface{}{
		"limited":      now.After(cl.limitedUntil) == false,
		"requests_mn":  cl.countInWindow(now, 1*time.Minute),
		"requests_hr":  cl.countInWindow(now, 1*time.Hour),
		"total":        len(cl.requests),
	}
}
