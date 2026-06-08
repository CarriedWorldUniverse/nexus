package hooks

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Handler runs a hook for a matching event payload.
type Handler interface {
	Run(context.Context, map[string]any) (Decision, error)
}

// HookEngine dispatches events to registered matcher groups.
type HookEngine struct {
	mu       sync.RWMutex
	registry map[string][]matcherGroup
}

type matcherGroup struct {
	matcher  matcher
	handlers []registeredHandler
}

type registeredHandler struct {
	handler Handler
	timeout time.Duration
}

type matcher struct {
	raw   string
	regex *regexp.Regexp
}

// New creates an empty hook engine.
func New() *HookEngine {
	return &HookEngine{registry: make(map[string][]matcherGroup)}
}

// Register appends handlers for an event and matcher in registration order.
func (e *HookEngine) Register(event, matcherText string, timeout time.Duration, handlers ...Handler) error {
	if e == nil {
		return errors.New("hooks: nil engine")
	}
	if strings.TrimSpace(event) == "" {
		return errors.New("hooks: event is required")
	}
	if len(handlers) == 0 {
		return nil
	}
	m, err := newMatcher(matcherText)
	if err != nil {
		return err
	}
	group := matcherGroup{matcher: m}
	for _, handler := range handlers {
		if handler == nil {
			return errors.New("hooks: nil handler")
		}
		group.handlers = append(group.handlers, registeredHandler{handler: handler, timeout: timeout})
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.registry[event] = append(e.registry[event], group)
	return nil
}

// Dispatch runs every registered handler whose event and matcher match payload.
func (e *HookEngine) Dispatch(ctx context.Context, event string, payload map[string]any) (Decision, error) {
	if e == nil {
		return defaultDecision(), errors.New("hooks: nil engine")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if payload == nil {
		payload = map[string]any{}
	}

	handlers := e.matchingHandlers(event, payload)
	if len(handlers) == 0 {
		return defaultDecision(), nil
	}

	decisions := make([]Decision, 0, len(handlers))
	var errs []error
	for _, handler := range handlers {
		decision, err := runIsolated(ctx, handler, payload)
		if err != nil {
			errs = append(errs, err)
		}
		decisions = append(decisions, decision)
	}

	return MergeDecisions(decisions...), errors.Join(errs...)
}

func (e *HookEngine) matchingHandlers(event string, payload map[string]any) []registeredHandler {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var handlers []registeredHandler
	target := matcherTarget(event, payload)
	for _, key := range []string{event, "*"} {
		for _, group := range e.registry[key] {
			if group.matcher.matches(target) {
				handlers = append(handlers, group.handlers...)
			}
		}
	}
	return handlers
}

func runIsolated(ctx context.Context, handler registeredHandler, payload map[string]any) (decision Decision, err error) {
	if handler.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, handler.timeout)
		defer cancel()
	}

	type result struct {
		decision Decision
		err      error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- result{err: fmt.Errorf("hooks: handler panic: %v", recovered)}
			}
		}()
		decision, err := handler.handler.Run(ctx, payload)
		done <- result{decision: decision, err: err}
	}()

	select {
	case result := <-done:
		return result.decision, result.err
	case <-ctx.Done():
		return Decision{}, fmt.Errorf("hooks: handler timeout: %w", ctx.Err())
	}
}

func newMatcher(raw string) (matcher, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "*" {
		return matcher{raw: raw}, nil
	}
	if hasRegexMeta(raw) {
		re, err := regexp.Compile(raw)
		if err != nil {
			return matcher{}, fmt.Errorf("hooks: invalid matcher regex %q: %w", raw, err)
		}
		return matcher{raw: raw, regex: re}, nil
	}
	return matcher{raw: raw}, nil
}

func (m matcher) matches(target string) bool {
	if m.raw == "" || m.raw == "*" {
		return true
	}
	if m.raw == target {
		return true
	}
	if m.regex != nil {
		return m.regex.MatchString(target)
	}
	return false
}

func matcherTarget(event string, payload map[string]any) string {
	for _, key := range []string{"tool_name", "toolName", "name"} {
		if value, ok := payload[key].(string); ok && value != "" {
			return value
		}
	}
	return event
}

func hasRegexMeta(value string) bool {
	return regexp.QuoteMeta(value) != value
}
