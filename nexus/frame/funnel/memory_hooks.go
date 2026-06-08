package funnel

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	funnelhooks "github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel/hooks"
)

const builtInMemoryHookTimeout = 5 * time.Second

var memoryCaptureMarkerRE = regexp.MustCompile(`(?ims)^\s*(?:Commonplace|Memory)\s*:\s*(?P<topic>[^\n]+)\n(?P<content>.+?)(?:\n\s*(?:---|\z)|\z)`)

func registerBuiltInMemoryHooks(engine *funnelhooks.HookEngine, cfg Config) {
	if engine == nil {
		return
	}
	if cfg.AutoRecall.Enabled && cfg.AutoRecall.Gateway != nil {
		_ = engine.Register("UserPromptSubmit", "*", builtInMemoryHookTimeout, readMemoryHook{cfg: cfg})
	}
	if cfg.AutoRecall.Gateway != nil {
		_ = engine.Register("Stop", "*", builtInMemoryHookTimeout, writeMemoryHook{cfg: cfg})
	}
}

func (f *Funnel) dispatchHooks(ctx context.Context, event string, st *deliberateState, payload map[string]any) string {
	if f.cfg.Hooks == nil {
		return ""
	}
	if payload == nil {
		payload = map[string]any{}
	}
	payload["hook_event_name"] = event
	for k, v := range f.hookBasePayload(st) {
		if _, exists := payload[k]; !exists {
			payload[k] = v
		}
	}
	decision, err := f.cfg.Hooks.Dispatch(ctx, event, payload)
	if err != nil {
		f.log.Debug("funnel: hook dispatch failed open", "event", event, "err", err)
	}
	return strings.TrimSpace(decision.AdditionalContext)
}

func (f *Funnel) hookBasePayload(st *deliberateState) map[string]any {
	payload := map[string]any{
		"session_id":     st.session.ID,
		"aspect_id":      f.cfg.AspectID,
		"cwd":            f.cfg.AspectHome,
		"trigger_msg_id": st.triggerMsgID,
		"trigger_from":   st.triggerFrom,
		"trigger_text":   st.triggerContent,
		"thread_root":    st.triggerThreadRoot,
		"turn_id":        st.turnID,
		"provider":       string(st.binding.Provider),
		"model":          st.binding.Model,
	}
	if st.trigger.Source != "" {
		payload["trigger_source"] = st.trigger.Source
	}
	return payload
}

type readMemoryHook struct {
	cfg Config
}

func (h readMemoryHook) Run(ctx context.Context, payload map[string]any) (funnelhooks.Decision, error) {
	message := stringPayload(payload, "message")
	block := recallForTurnConfig(ctx, h.cfg, message)
	if block == "" {
		return funnelhooks.Decision{}, nil
	}
	return funnelhooks.Decision{AdditionalContext: block}, nil
}

type writeMemoryHook struct {
	cfg Config
}

func (h writeMemoryHook) Run(ctx context.Context, payload map[string]any) (funnelhooks.Decision, error) {
	gateway := h.cfg.AutoRecall.Gateway
	if gateway == nil {
		return funnelhooks.Decision{}, nil
	}
	finalText := stringPayload(payload, "final_text")
	for _, item := range extractMemoryCaptures(finalText) {
		if _, err := gateway.StoreKnowledge(ctx, h.cfg.AspectID, item.topic, item.content, false); err != nil {
			logger := h.cfg.Logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.Warn("memory hook: store failed; proceeding", "topic", item.topic, "err", err)
			return funnelhooks.Decision{}, nil
		}
	}
	return funnelhooks.Decision{}, nil
}

type memoryCapture struct {
	topic   string
	content string
}

func extractMemoryCaptures(text string) []memoryCapture {
	matches := memoryCaptureMarkerRE.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	topicIndex := memoryCaptureMarkerRE.SubexpIndex("topic")
	contentIndex := memoryCaptureMarkerRE.SubexpIndex("content")
	out := make([]memoryCapture, 0, len(matches))
	for _, match := range matches {
		topic := strings.TrimSpace(match[topicIndex])
		content := strings.TrimSpace(match[contentIndex])
		if topic == "" || content == "" {
			continue
		}
		out = append(out, memoryCapture{topic: topic, content: content})
	}
	return out
}

func stringPayload(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	switch v := payload[key].(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	default:
		return ""
	}
}
