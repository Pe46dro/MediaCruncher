package notification

import (
	"fmt"
	"sync"
)

// RoutingRule defines how events are routed to channels.
type RoutingRule struct {
	EventTypes []EventType       `json:"event_types"`
	Severities []Severity        `json:"severities"`
	Channels   []string          `json:"channels"`
	Batched    bool              `json:"batched"`
	Priority   int               `json:"priority"`
	Conditions map[string]string `json:"conditions,omitempty"`
}

// Router routes events to configured channel adapters.
type Router struct {
	mu       sync.RWMutex
	rules    []RoutingRule
	channels map[string]string
}

// NewRouter creates a new event router.
func NewRouter() *Router {
	return &Router{
		rules:    make([]RoutingRule, 0),
		channels: make(map[string]string),
	}
}

// AddRule adds a routing rule.
func (r *Router) AddRule(rule RoutingRule) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rules = append(r.rules, rule)
}

// AddChannel registers a channel name.
func (r *Router) AddChannel(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.channels[name] = name
}

// Route returns the list of channels an event should be sent to.
func (r *Router) Route(evt *Event) []ChannelRoute {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var routes []ChannelRoute

	for _, rule := range r.rules {
		if !r.matchesRule(evt, rule) {
			continue
		}

		for _, channel := range rule.Channels {
			routes = append(routes, ChannelRoute{
				Channel: channel,
				Batched: rule.Batched,
			})
		}
	}

	if len(routes) == 0 {
		return []ChannelRoute{{Channel: "default", Batched: true}}
	}

	return routes
}

// matchesRule checks if an event matches a routing rule.
func (r *Router) matchesRule(evt *Event, rule RoutingRule) bool {
	for _, et := range rule.EventTypes {
		if evt.Type == et {
			break
		}
	}

	for _, sev := range rule.Severities {
		if evt.Severity == sev {
			break
		}
	}

	if len(rule.Conditions) > 0 && evt.Context != nil {
		for k, v := range rule.Conditions {
			if ctxVal, ok := evt.Context[k]; ok {
				if fmt.Sprintf("%v", ctxVal) != v {
					return false
				}
			} else {
				return false
			}
		}
	}

	return true
}

// GetChannels returns all registered channels.
func (r *Router) GetChannels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	channels := make([]string, 0, len(r.channels))
	for name := range r.channels {
		channels = append(channels, name)
	}
	return channels
}

// GetBatchedChannels returns channels that use batching for the event.
func (r *Router) GetBatchedChannels(evt *Event) []string {
	routes := r.Route(evt)
	var batched []string
	for _, route := range routes {
		if route.Batched {
			batched = append(batched, route.Channel)
		}
	}
	return batched
}

// GetImmediateChannels returns channels that should receive immediate delivery.
func (r *Router) GetImmediateChannels(evt *Event) []string {
	routes := r.Route(evt)
	var immediate []string
	for _, route := range routes {
		if !route.Batched {
			immediate = append(immediate, route.Channel)
		}
	}
	return immediate
}

// ChannelRoute represents a routing decision for an event.
type ChannelRoute struct {
	Channel string
	Batched bool
}
