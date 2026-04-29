package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// fileStore is a tiny disk-backed casket.ChannelStorage. Each key
// becomes a file under <root>/<nexusID>/<sanitized-key>. Values are raw
// strings; casket already base64-encodes binary material before calling
// Put. PoC quality — no fsync, no locking, no permission tightening
// beyond os defaults. Sufficient for an operator-driven CLI run on a
// trusted host.
type fileStore struct{ dir string }

func openStore(root, nexusID string) (*fileStore, error) {
	if root == "" || nexusID == "" {
		return nil, errors.New("store: root and nexus-id required")
	}
	dir := filepath.Join(root, nexusID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store mkdir: %w", err)
	}
	return &fileStore{dir: dir}, nil
}

func (s *fileStore) path(key string) string {
	return filepath.Join(s.dir, sanitize(key))
}

func (s *fileStore) Get(_ context.Context, key string) (string, error) {
	b, err := os.ReadFile(s.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (s *fileStore) Put(_ context.Context, key, value string) error {
	return os.WriteFile(s.path(key), []byte(value), 0o600)
}

func (s *fileStore) Delete(_ context.Context, key string) error {
	err := os.Remove(s.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// sanitize swaps separators that would escape the per-id directory.
// Casket keys are well-formed, but defense-in-depth.
func sanitize(s string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", "..", "__", ":", "_")
	return r.Replace(s)
}

// insecureHTTP returns an http.Client that skips TLS verification.
// The CLI talks to Funnel-fronted hosts whose certs may not be in the
// system trust store, and to local tailnet IPs over plain HTTP. This
// is appropriate for a dev CLI; production callers should configure
// their own *http.Client with proper roots.
func insecureHTTP() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

// bytes is imported in main.go; re-export the buffer alias here so the
// compiler is happy when only this file uses it. Saves an extra import.
var _ = bytes.MinRead
