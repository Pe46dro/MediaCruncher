package web

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// EventMessage represents an SSE event dispatched to clients.
type EventMessage struct {
	ID        string      `json:"id"`
	Type      string      `json:"type"`      // e.g. "scan_started", "transcode_started", "quality_rejected", "transcode_completed"
	Severity  string      `json:"severity"`  // "info", "success", "warning", "error"
	Message   string      `json:"message"`   // Human readable summary
	Timestamp time.Time   `json:"timestamp"`
	Data      interface{} `json:"data,omitempty"`
}

// Broker manages active Server-Sent Events (SSE) connections and broadcasts events.
type Broker struct {
	mu           sync.RWMutex
	clients      map[chan []byte]bool
	recentEvents []EventMessage
	maxRecent    int
	stopCh       chan struct{}
}

// NewBroker creates a new SSE event broker.
func NewBroker() *Broker {
	b := &Broker{
		clients:      make(map[chan []byte]bool),
		recentEvents: make([]EventMessage, 0, 100),
		maxRecent:    100,
		stopCh:       make(chan struct{}),
	}
	go b.heartbeat()
	return b
}

// Subscribe registers a new client channel. Returns the channel and a cleanup unsubscribe function.
func (b *Broker) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 32)

	b.mu.Lock()
	b.clients[ch] = true
	// Send recent events to new client immediately
	recent := make([]EventMessage, len(b.recentEvents))
	copy(recent, b.recentEvents)
	b.mu.Unlock()

	for _, evt := range recent {
		payload, err := formatSSE(evt.Type, evt)
		if err == nil {
			select {
			case ch <- payload:
			default:
			}
		}
	}

	unsubscribe := func() {
		b.mu.Lock()
		if b.clients[ch] {
			delete(b.clients, ch)
			close(ch)
		}
		b.mu.Unlock()
	}

	return ch, unsubscribe
}

// Broadcast dispatches an event to all connected clients.
func (b *Broker) Broadcast(eventType, severity, message string, data interface{}) {
	evt := EventMessage{
		ID:        fmt.Sprintf("evt-%d", time.Now().UnixNano()),
		Type:      eventType,
		Severity:  severity,
		Message:   message,
		Timestamp: time.Now(),
		Data:      data,
	}

	payload, err := formatSSE(eventType, evt)
	if err != nil {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// Append to ring buffer
	if len(b.recentEvents) >= b.maxRecent {
		b.recentEvents = b.recentEvents[1:]
	}
	b.recentEvents = append(b.recentEvents, evt)

	for ch := range b.clients {
		select {
		case ch <- payload:
		default:
			// Client slow, skip to avoid blocking other clients
		}
	}
}

// GetRecentEvents returns a copy of recently broadcast events.
func (b *Broker) GetRecentEvents() []EventMessage {
	b.mu.RLock()
	defer b.mu.RUnlock()
	recent := make([]EventMessage, len(b.recentEvents))
	copy(recent, b.recentEvents)
	return recent
}

// Close stops the broker and closes all client connections.
func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case <-b.stopCh:
	default:
		close(b.stopCh)
	}
	for ch := range b.clients {
		close(ch)
		delete(b.clients, ch)
	}
}

// heartbeat sends a keep-alive ping comment to all connected SSE clients every 15 seconds.
func (b *Broker) heartbeat() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-b.stopCh:
			return
		case <-ticker.C:
			b.mu.RLock()
			ping := []byte(": keepalive\n\n")
			for ch := range b.clients {
				select {
				case ch <- ping:
				default:
				}
			}
			b.mu.RUnlock()
		}
	}
}

func formatSSE(event string, data interface{}) ([]byte, error) {
	bytesData, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, string(bytesData))), nil
}
