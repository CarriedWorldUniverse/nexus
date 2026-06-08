package hooks

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// Config mirrors hooks.json and can also be constructed inline.
type Config struct {
	Hooks map[string][]MatcherConfig `json:"hooks"`
}

type MatcherConfig struct {
	Matcher string          `json:"matcher,omitempty"`
	Hooks   []HandlerConfig `json:"hooks"`
}

type HandlerConfig struct {
	Type    string `json:"type"`
	Command string `json:"command,omitempty"`
	URL     string `json:"url,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Timeout int    `json:"timeout,omitempty"`
}

type LoadOptions struct {
	MCPInvoker MCPInvoker
	HTTPClient *http.Client
}

// LoadConfig builds an engine from one config source. Layered source merging
// can be added above this function before registration.
func LoadConfig(cfg Config, opts *LoadOptions) (*HookEngine, error) {
	if opts == nil {
		opts = &LoadOptions{}
	}
	engine := New()
	for event, groups := range cfg.Hooks {
		for _, group := range groups {
			for _, hook := range group.Hooks {
				handler, err := handlerFromConfig(hook, *opts)
				if err != nil {
					return nil, fmt.Errorf("hooks: %s matcher %q: %w", event, group.Matcher, err)
				}
				if err := engine.Register(event, group.Matcher, time.Duration(hook.Timeout)*time.Second, handler); err != nil {
					return nil, err
				}
			}
		}
	}
	return engine, nil
}

func LoadFile(path string, opts LoadOptions) (*HookEngine, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("hooks: read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("hooks: parse config: %w", err)
	}
	return LoadConfig(cfg, &opts)
}

func handlerFromConfig(cfg HandlerConfig, opts LoadOptions) (Handler, error) {
	switch cfg.Type {
	case "command":
		if cfg.Command == "" {
			return nil, fmt.Errorf("command is required")
		}
		return NewCommandHandler(cfg.Command), nil
	case "http":
		if cfg.URL == "" {
			return nil, fmt.Errorf("url is required")
		}
		handler := NewHTTPHandler(cfg.URL)
		if opts.HTTPClient != nil {
			handler.Client = opts.HTTPClient
		}
		return handler, nil
	case "mcp_tool":
		if cfg.Tool == "" {
			return nil, fmt.Errorf("tool is required")
		}
		return NewMCPToolHandler(cfg.Tool, opts.MCPInvoker), nil
	default:
		return nil, fmt.Errorf("unknown hook type %q", cfg.Type)
	}
}
