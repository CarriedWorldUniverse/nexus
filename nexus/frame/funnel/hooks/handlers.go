package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
)

// CommandHandler runs a local command with the JSON payload on stdin.
type CommandHandler struct {
	Command string
}

func NewCommandHandler(command string) CommandHandler {
	return CommandHandler{Command: command}
}

func (h CommandHandler) Run(ctx context.Context, payload map[string]any) (Decision, error) {
	if strings.TrimSpace(h.Command) == "" {
		return Decision{}, fmt.Errorf("hooks: command is required")
	}

	stdin, err := json.Marshal(payload)
	if err != nil {
		return Decision{}, fmt.Errorf("hooks: marshal command payload: %w", err)
	}

	cmd := shellCommand(ctx, h.Command)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	if err == nil {
		var decision Decision
		if strings.TrimSpace(stdout.String()) == "" {
			return Decision{}, nil
		}
		if decodeErr := json.Unmarshal(stdout.Bytes(), &decision); decodeErr != nil {
			return Decision{}, fmt.Errorf("hooks: parse command decision: %w", decodeErr)
		}
		return decision, nil
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return Decision{}, fmt.Errorf("hooks: run command: %w", err)
	}
	reason := strings.TrimSpace(stderr.String())
	if exitErr.ExitCode() == 2 {
		return Decision{
			Decision:           DecisionBlock,
			PermissionDecision: DecisionBlock,
			SystemMessage:      reason,
		}.WithContinue(false), nil
	}
	if reason == "" {
		reason = err.Error()
	}
	return Decision{}, fmt.Errorf("hooks: command exited %d: %s", exitErr.ExitCode(), reason)
}

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/C", command)
	}
	return exec.CommandContext(ctx, "/bin/sh", "-c", command)
}

// HTTPHandler posts the JSON payload to a hook URL and parses a JSON Decision.
type HTTPHandler struct {
	URL    string
	Client *http.Client
}

func NewHTTPHandler(url string) HTTPHandler {
	return HTTPHandler{URL: url, Client: http.DefaultClient}
}

func (h HTTPHandler) Run(ctx context.Context, payload map[string]any) (Decision, error) {
	if strings.TrimSpace(h.URL) == "" {
		return Decision{}, fmt.Errorf("hooks: http url is required")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Decision{}, fmt.Errorf("hooks: marshal http payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		return Decision{}, fmt.Errorf("hooks: create http request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return Decision{}, fmt.Errorf("hooks: post hook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Decision{}, fmt.Errorf("hooks: http hook status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var decision Decision
	if err := json.NewDecoder(resp.Body).Decode(&decision); err != nil && err != io.EOF {
		return Decision{}, fmt.Errorf("hooks: parse http decision: %w", err)
	}
	return decision, nil
}

// MCPInvoker is the seam used by mcp_tool hooks. Funnel wiring provides the real invoker later.
type MCPInvoker interface {
	InvokeMCPTool(context.Context, string, map[string]any) (Decision, error)
}

type MCPInvokerFunc func(context.Context, string, map[string]any) (Decision, error)

func (f MCPInvokerFunc) InvokeMCPTool(ctx context.Context, tool string, payload map[string]any) (Decision, error) {
	return f(ctx, tool, payload)
}

type StubMCPInvoker struct{}

func (StubMCPInvoker) InvokeMCPTool(context.Context, string, map[string]any) (Decision, error) {
	return Decision{}, nil
}

type MCPToolHandler struct {
	Tool    string
	Invoker MCPInvoker
}

func NewMCPToolHandler(tool string, invoker MCPInvoker) MCPToolHandler {
	if invoker == nil {
		invoker = StubMCPInvoker{}
	}
	return MCPToolHandler{Tool: tool, Invoker: invoker}
}

func (h MCPToolHandler) Run(ctx context.Context, payload map[string]any) (Decision, error) {
	if strings.TrimSpace(h.Tool) == "" {
		return Decision{}, fmt.Errorf("hooks: mcp tool is required")
	}
	invoker := h.Invoker
	if invoker == nil {
		invoker = StubMCPInvoker{}
	}
	return invoker.InvokeMCPTool(ctx, h.Tool, payload)
}
