package main

import (
	"context"
	"log/slog"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

const (
	defaultImageQuestion = "Describe this image in detail, including any on-screen text, HUD, or UI."
	defaultVideoQuestion = "These frames are sampled in order from a video. Describe what is shown and what changes across them."
	maxVideoFrames       = 8
	defaultVideoFrames   = 6
)

// registerTools wires read_image and read_video onto the MCP server. Both
// route to the configured local vision model and return a plain-text
// description; errors are returned as tool errors (the agent sees the reason
// and can retry or fall back), never as a process crash.
func registerTools(srv *mcpserver.MCPServer, log *slog.Logger, cfg visionConfig) {
	srv.AddTool(mcpgo.NewTool("read_image",
		mcpgo.WithDescription("SEE an image and get a text description. Give a local file path (in this worker's filesystem, e.g. a screenshot or render) or an http(s) URL. Optionally focus the reading with a question. Routes to the local vision model."),
		mcpgo.WithString("path", mcpgo.Required(), mcpgo.Description("Local file path or http(s) URL of the image.")),
		mcpgo.WithString("question", mcpgo.Description("Optional focus, e.g. 'what does the HUD say?' or 'is the water flowing?'")),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		path := strings.TrimSpace(req.GetString("path", ""))
		if path == "" {
			return mcpgo.NewToolResultError("path is required"), nil
		}
		question := firstNonEmpty(req.GetString("question", ""), defaultImageQuestion)
		uri, err := loadImageDataURI(ctx, path)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		desc, err := describeImages(ctx, cfg, question, []string{uri})
		if err != nil {
			log.Warn("read_image failed", "path", path, "err", err)
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		return mcpgo.NewToolResultText(desc), nil
	})

	srv.AddTool(mcpgo.NewTool("read_video",
		mcpgo.WithDescription("SEE a video by sampling frames and describing them. Give a local file path (this worker's filesystem) and optionally a question and frame count (default 6, max 8). The vision model reads stills, so this samples evenly-spaced frames and describes the sequence. Requires ffmpeg in the image."),
		mcpgo.WithString("path", mcpgo.Required(), mcpgo.Description("Local file path of the video.")),
		mcpgo.WithString("question", mcpgo.Description("Optional focus for the description.")),
		mcpgo.WithNumber("frames", mcpgo.Description("How many frames to sample (default 6, max 8).")),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		path := strings.TrimSpace(req.GetString("path", ""))
		if path == "" {
			return mcpgo.NewToolResultError("path is required"), nil
		}
		question := firstNonEmpty(req.GetString("question", ""), defaultVideoQuestion)
		frames := clampFrames(int(req.GetFloat("frames", defaultVideoFrames)))
		uris, err := extractVideoFrames(ctx, path, frames)
		if err != nil {
			log.Warn("read_video frame extraction failed", "path", path, "err", err)
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		desc, err := describeImages(ctx, cfg, question, uris)
		if err != nil {
			log.Warn("read_video describe failed", "path", path, "err", err)
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		return mcpgo.NewToolResultText(desc), nil
	})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// clampFrames bounds the requested frame count to [1, maxVideoFrames],
// defaulting a non-positive request to defaultVideoFrames.
func clampFrames(n int) int {
	if n <= 0 {
		return defaultVideoFrames
	}
	if n > maxVideoFrames {
		return maxVideoFrames
	}
	return n
}
