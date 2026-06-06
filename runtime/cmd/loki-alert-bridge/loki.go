package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

type LokiClient struct {
	BaseURL string
	HTTP    *http.Client
}

func (c *LokiClient) QueryRange(ctx context.Context, query string, start, end time.Time, limit int) ([]string, error) {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	base, err := url.Parse(c.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse Loki URL: %w", err)
	}
	base.Path = joinURLPath(base.Path, "/loki/api/v1/query_range")
	q := base.Query()
	q.Set("query", query)
	q.Set("start", strconv.FormatInt(start.UnixNano(), 10))
	q.Set("end", strconv.FormatInt(end.UnixNano(), 10))
	q.Set("direction", "backward")
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	base.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build Loki request: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query Loki: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("query Loki: status %d", resp.StatusCode)
	}
	var out lokiQueryRangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode Loki response: %w", err)
	}
	lines := make([]lokiLine, 0)
	for _, stream := range out.Data.Result {
		for _, pair := range stream.Values {
			if len(pair) < 2 {
				continue
			}
			ts, _ := strconv.ParseInt(pair[0], 10, 64)
			lines = append(lines, lokiLine{ts: ts, text: pair[1]})
		}
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].ts < lines[j].ts })
	text := make([]string, 0, len(lines))
	for _, line := range lines {
		text = append(text, line.text)
	}
	return text, nil
}

type lokiQueryRangeResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Stream map[string]string `json:"stream"`
			Values [][]string        `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

type lokiLine struct {
	ts   int64
	text string
}

func joinURLPath(basePath, suffix string) string {
	if basePath == "" || basePath == "/" {
		return suffix
	}
	if basePath[len(basePath)-1] == '/' {
		basePath = basePath[:len(basePath)-1]
	}
	return basePath + suffix
}
