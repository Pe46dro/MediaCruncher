package concurrency

import "fmt"

// Route defines how a result is routed after processing.
type Route struct {
	SourceJobType JobType
	Conditions    map[string]interface{}
	NextJobType   JobType
	Action        RouteAction
	Priority      Priority
	ShouldRetry   bool
}

// RouteAction defines what action to take after routing.
type RouteAction string

const (
	RouteActionEnqueue RouteAction = "enqueue"
	RouteActionComplete RouteAction = "complete"
	RouteActionNotify  RouteAction = "notify"
	RouteActionRetry   RouteAction = "retry"
	RouteActionFail    RouteAction = "fail"
)

// Router determines where to send a result based on the source task and outcome.
type Router struct {
	routes   []Route
	defaultAction RouteAction
}

// NewRouter creates a new router with default configuration.
func NewRouter(defaultAction RouteAction) *Router {
	if defaultAction == "" {
		defaultAction = RouteActionComplete
	}
	return &Router{
		routes:        make([]Route, 0),
		defaultAction: defaultAction,
	}
}

// AddRoute adds a routing rule.
func (r *Router) AddRoute(route Route) {
	r.routes = append(r.routes, route)
}

// Route determines the next action for a result.
func (r *Router) Route(task *Task, result *Result) *RoutingDecision {
	if task == nil || result == nil {
		return &RoutingDecision{
			Action: r.defaultAction,
			Reason: "nil task or result",
		}
	}

	for _, route := range r.routes {
		if route.SourceJobType != task.JobType {
			continue
		}

		match := true
		for key, expected := range route.Conditions {
			if result.Data != nil {
				if dataMap, ok := result.Data.(map[string]interface{}); ok {
					if val, exists := dataMap[key]; exists {
						if val != expected {
							match = false
							break
						}
					} else {
						match = false
						break
					}
				}
			}
		}

		if match {
			decision := &RoutingDecision{
				Action:      route.Action,
				NextJobType: route.NextJobType,
				Priority:    route.Priority,
				ShouldRetry: route.ShouldRetry,
				Reason:      fmt.Sprintf("matched route for %s with conditions %v", task.JobType, route.Conditions),
			}
			return decision
		}
	}

	return &RoutingDecision{
		Action: r.defaultAction,
		Reason: "no matching route, using default",
	}
}

// SetDefaultAction sets the default action when no route matches.
func (r *Router) SetDefaultAction(action RouteAction) {
	r.defaultAction = action
}

// GetRoutes returns all configured routes.
func (r *Router) GetRoutes() []Route {
	return r.routes
}

// RoutingDecision holds the outcome of a routing evaluation.
type RoutingDecision struct {
	Action      RouteAction
	NextJobType JobType
	Priority    Priority
	ShouldRetry bool
	Reason      string
}

// ShouldEnqueue returns true if the decision is to enqueue for further processing.
func (d *RoutingDecision) ShouldEnqueue() bool {
	return d.Action == RouteActionEnqueue || d.Action == RouteActionNotify
}

// ShouldFail returns true if the decision is to permanently fail.
func (d *RoutingDecision) ShouldFail() bool {
	return d.Action == RouteActionFail
}

// ShouldRetry returns true if the decision is to retry.
func (d *RoutingDecision) ShouldRetryTask() bool {
	return d.Action == RouteActionRetry || d.ShouldRetry
}
