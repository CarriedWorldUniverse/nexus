package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// visionConfig is the resolved endpoint the tools call.
type visionConfig struct {
	BaseURL string
	Model   string
	APIKey  string
}

func visionConfigFromEnv() visionConfig {
	return visionConfig{
		BaseURL: envOr("VISION_BASE_URL", "http://litellm.model-stack.svc.cluster.local:4000/v1"),
		Model:   envOr("VISION_MODEL", "vision"),
		APIKey:  envOr("VISION_API_KEY", "dummy"),
	}
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// httpTimeout bounds a single vision call — a multimodal turn on a local GPU
// can take a while, but a hung endpoint must not wedge the agent.
const httpTimeout = 120 * time.Second

// maxImageBytes caps a single decoded image (before base64) so a runaway
// path can't OOM the tool or blow the model's context.
const maxImageBytes = 12 << 20 // 12 MiB

// describeImages sends question + one-or-more images to the OpenAI-compatible
// vision endpoint and returns the assistant's text. Images are data: URIs.
func describeImages(ctx context.Context, cfg visionConfig, question string, imageDataURIs []string) (string, error) {
	if len(imageDataURIs) == 0 {
		return "", fmt.Errorf("no image provided")
	}
	content := make([]map[string]any, 0, len(imageDataURIs)+1)
	content = append(content, map[string]any{"type": "text", "text": question})
	for _, uri := range imageDataURIs {
		content = append(content, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": uri},
		})
	}
	body, _ := json.Marshal(map[string]any{
		"model":      cfg.Model,
		"max_tokens": 1024,
		"messages":   []map[string]any{{"role": "user", "content": content}},
	})

	reqCtx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		strings.TrimRight(cfg.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("vision endpoint unreachable (%s): %w", cfg.BaseURL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("vision endpoint status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return parseVisionContent(raw)
}

// parseVisionContent extracts choices[0].message.content from an
// OpenAI-shape chat completion. Pure, for testing.
func parseVisionContent(raw []byte) (string, error) {
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("parse vision response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("vision response had no choices")
	}
	text := strings.TrimSpace(out.Choices[0].Message.Content)
	if text == "" {
		return "", fmt.Errorf("vision model returned empty content")
	}
	return text, nil
}

// loadImageDataURI reads a local path or http(s) URL and returns a
// data:<mime>;base64,<...> URI capped at maxImageBytes.
func loadImageDataURI(ctx context.Context, pathOrURL string) (string, error) {
	var data []byte
	var err error
	if strings.HasPrefix(pathOrURL, "http://") || strings.HasPrefix(pathOrURL, "https://") {
		data, err = fetchURL(ctx, pathOrURL)
	} else {
		data, err = readFileCapped(pathOrURL)
	}
	if err != nil {
		return "", err
	}
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		return "", fmt.Errorf("%s is not an image (detected %s)", pathOrURL, mime)
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func readFileCapped(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read image: %w", err)
	}
	if fi.Size() > maxImageBytes {
		return nil, fmt.Errorf("image %s is %d bytes, over the %d cap", path, fi.Size(), maxImageBytes)
	}
	return os.ReadFile(path)
}

func fetchURL(ctx context.Context, url string) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: status %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
}

// frameTimestamps returns n evenly-spaced timestamps (seconds) strictly
// inside a clip of the given duration — the sample points for read_video.
// Pure, for testing.
func frameTimestamps(duration float64, n int) []float64 {
	if n < 1 {
		n = 1
	}
	ts := make([]float64, 0, n)
	for i := 1; i <= n; i++ {
		ts = append(ts, duration*float64(i)/float64(n+1))
	}
	return ts
}

// extractVideoFrames samples n frames from a local video via ffmpeg and
// returns them as image data URIs. Requires ffmpeg + ffprobe in the image;
// a missing binary yields a clear error (video is best-effort until the
// worker image ships ffmpeg).
func extractVideoFrames(ctx context.Context, path string, n int) ([]string, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("read_video needs ffmpeg, which is not installed in this image")
	}
	dur, err := probeDuration(ctx, path)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "vision-frames-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	var uris []string
	for i, t := range frameTimestamps(dur, n) {
		out := filepath.Join(tmp, fmt.Sprintf("f%02d.jpg", i))
		cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-y",
			"-ss", fmt.Sprintf("%.3f", t), "-i", path, "-frames:v", "1", "-q:v", "3", out)
		if err := cmd.Run(); err != nil {
			continue // skip an unreadable frame rather than fail the whole call
		}
		uri, err := loadImageDataURI(ctx, out)
		if err == nil {
			uris = append(uris, uri)
		}
	}
	if len(uris) == 0 {
		return nil, fmt.Errorf("ffmpeg extracted no readable frames from %s", path)
	}
	return uris, nil
}

// probeDuration reads a clip's duration (seconds) via ffprobe.
func probeDuration(ctx context.Context, path string) (float64, error) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return 0, fmt.Errorf("read_video needs ffprobe, which is not installed in this image")
	}
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error",
		"-show_entries", "format=duration", "-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe %s: %w", path, err)
	}
	var d float64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%f", &d); err != nil || d <= 0 {
		return 0, fmt.Errorf("could not read duration of %s", path)
	}
	return d, nil
}
