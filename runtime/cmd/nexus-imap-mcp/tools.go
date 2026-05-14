// MCP tool registration + handlers. Thin adapters: parse arguments,
// call the matching Client method, shape the result into a
// CallToolResult.

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func registerTools(srv *mcpserver.MCPServer, c *Client, defaultFolder string, log *slog.Logger) {
	type toolDef struct {
		name        string
		description string
		schema      mcpgo.ToolInputSchema
		handler     func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)
	}

	tools := []toolDef{
		{
			name:        "imap.list_folders",
			description: "List every mailbox folder the authenticated user can see. Useful before move/fetch when the folder name isn't known.",
			schema:      mcpgo.ToolInputSchema{Type: "object", Properties: map[string]any{}},
			handler: func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				folders, err := c.ListFolders(ctx)
				if err != nil {
					return mcpErr(err.Error()), nil
				}
				return mcpJSON(map[string]any{"folders": folders}), nil
			},
		},
		{
			name:        "imap.fetch_recent",
			description: "Fetch recent messages from a folder, newest first. Optional filters: from/subject substring, since (RFC 3339 date), limit (1-100, default 20). Body returned as plain-text excerpt (first 8 KiB).",
			schema: mcpgo.ToolInputSchema{
				Type: "object",
				Properties: map[string]any{
					"folder":  map[string]any{"type": "string", "description": "Folder name (default: keyfile.imap.default_folder or INBOX)."},
					"from":    map[string]any{"type": "string", "description": "Substring/email match on the From header (server-side)."},
					"subject": map[string]any{"type": "string", "description": "Substring match on the Subject header."},
					"since":   map[string]any{"type": "string", "description": "RFC 3339 timestamp — only messages newer than this."},
					"limit":   map[string]any{"type": "integer", "description": "Max messages (1-100, default 20)."},
				},
			},
			handler: func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				folder := req.GetString("folder", defaultFolder)
				from := req.GetString("from", "")
				subject := req.GetString("subject", "")
				sinceStr := req.GetString("since", "")
				limit := int(req.GetFloat("limit", 0))
				var since time.Time
				if sinceStr != "" {
					t, err := time.Parse(time.RFC3339, sinceStr)
					if err != nil {
						return mcpErr("since must be RFC 3339: " + err.Error()), nil
					}
					since = t
				}
				msgs, err := c.FetchRecent(ctx, folder, limit, from, subject, since)
				if err != nil {
					return mcpErr(err.Error()), nil
				}
				return mcpJSON(map[string]any{"messages": msgs}), nil
			},
		},
		{
			name:        "imap.fetch_otp",
			description: "Scan a folder for the most recent message from a sender (substring match) within max_age_seconds and return the first 6-digit numeric code found. Useful for first-login OTP flows (Atlassian, etc.). Returns the code + the matching message.",
			schema: mcpgo.ToolInputSchema{
				Type: "object",
				Properties: map[string]any{
					"folder":          map[string]any{"type": "string", "description": "Folder to scan (default: keyfile default)."},
					"sender":          map[string]any{"type": "string", "description": "Substring match on From. Empty = any sender."},
					"max_age_seconds": map[string]any{"type": "integer", "description": "How far back to look (default 600 = 10 min)."},
				},
			},
			handler: func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				folder := req.GetString("folder", defaultFolder)
				sender := req.GetString("sender", "")
				maxAge := time.Duration(int(req.GetFloat("max_age_seconds", 600))) * time.Second
				if maxAge <= 0 {
					maxAge = 10 * time.Minute
				}
				code, ref, err := c.FetchOTP(ctx, folder, sender, maxAge)
				if err != nil {
					return mcpErr(err.Error()), nil
				}
				return mcpJSON(map[string]any{"code": code, "message": ref}), nil
			},
		},
		{
			name:        "imap.move",
			description: "Move a message UID from src folder to dst. Auto-creates dst if it doesn't exist. Used to file processed mail (e.g. completed OTPs → \"otp/used\") so the inbox stays tidy.",
			schema: mcpgo.ToolInputSchema{
				Type: "object",
				Properties: map[string]any{
					"src": map[string]any{"type": "string", "description": "Source folder (default: keyfile default)."},
					"uid": map[string]any{"type": "integer", "description": "Message UID returned by fetch_recent/fetch_otp."},
					"dst": map[string]any{"type": "string", "description": "Destination folder. Created if missing."},
				},
				Required: []string{"uid", "dst"},
			},
			handler: func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				src := req.GetString("src", defaultFolder)
				dst := req.GetString("dst", "")
				uid := uint32(req.GetFloat("uid", 0))
				if dst == "" || uid == 0 {
					return mcpErr("uid and dst are required"), nil
				}
				if err := c.Move(ctx, src, uid, dst); err != nil {
					return mcpErr(err.Error()), nil
				}
				return mcpJSON(map[string]any{"moved": uid, "from": src, "to": dst}), nil
			},
		},
		{
			name:        "imap.delete",
			description: "Mark UID as \\Deleted and expunge. One-shot — caller doesn't need to call expunge separately.",
			schema: mcpgo.ToolInputSchema{
				Type: "object",
				Properties: map[string]any{
					"folder": map[string]any{"type": "string"},
					"uid":    map[string]any{"type": "integer"},
				},
				Required: []string{"uid"},
			},
			handler: func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				folder := req.GetString("folder", defaultFolder)
				uid := uint32(req.GetFloat("uid", 0))
				if uid == 0 {
					return mcpErr("uid is required"), nil
				}
				if err := c.Delete(ctx, folder, uid); err != nil {
					return mcpErr(err.Error()), nil
				}
				return mcpJSON(map[string]any{"deleted": uid, "folder": folder}), nil
			},
		},
		{
			name:        "imap.mark",
			description: "Set or clear IMAP flags on a message UID. Standard flags: \\Seen, \\Flagged, \\Answered, \\Draft. Pass add+remove arrays.",
			schema: mcpgo.ToolInputSchema{
				Type: "object",
				Properties: map[string]any{
					"folder": map[string]any{"type": "string"},
					"uid":    map[string]any{"type": "integer"},
					"add":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"remove": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				Required: []string{"uid"},
			},
			handler: func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				folder := req.GetString("folder", defaultFolder)
				uid := uint32(req.GetFloat("uid", 0))
				add := req.GetStringSlice("add", nil)
				remove := req.GetStringSlice("remove", nil)
				if uid == 0 {
					return mcpErr("uid is required"), nil
				}
				if len(add) == 0 && len(remove) == 0 {
					return mcpErr("at least one of add/remove must be non-empty"), nil
				}
				if err := c.Mark(ctx, folder, uid, add, remove); err != nil {
					return mcpErr(err.Error()), nil
				}
				return mcpJSON(map[string]any{"marked": uid, "folder": folder, "added": add, "removed": remove}), nil
			},
		},
	}

	for _, t := range tools {
		tool := mcpgo.Tool{
			Name:        t.name,
			Description: t.description,
			InputSchema: t.schema,
		}
		srv.AddTool(tool, t.handler)
		log.Debug("registered tool", "name", t.name)
	}
}

func mcpJSON(v any) *mcpgo.CallToolResult {
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcpErr("internal: encode result: " + err.Error())
	}
	return &mcpgo.CallToolResult{
		Content: []mcpgo.Content{
			mcpgo.TextContent{Type: "text", Text: string(buf)},
		},
	}
}

func mcpErr(msg string) *mcpgo.CallToolResult {
	return &mcpgo.CallToolResult{
		IsError: true,
		Content: []mcpgo.Content{
			mcpgo.TextContent{Type: "text", Text: msg},
		},
	}
}
