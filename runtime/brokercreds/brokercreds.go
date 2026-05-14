// Package brokercreds is the aspect-side client helper for fetching
// kind-typed credentials from the Nexus broker via the credential.fetch
// WS frame (NEX-77).
//
// Use case: nexus-jira-mcp, nexus-imap-mcp, and future per-kind MCPs
// pull their service credentials from the broker at startup instead of
// reading them from the on-disk keyfile. The keyfile still carries
// non-secret config (project_key, default_folder, etc.) but the secrets
// themselves live in the broker's encrypted credentials store.
//
// Contract:
//   - One Request → one .result or one .error response, correlated by
//     envelope ID. wsclient.Client handles correlation; this package
//     just wraps the kind-typed payload + error shape.
//   - Empty Name asks the broker to resolve the aspect's default
//     credential for the requested kind (aspects.default_<kind>_credential).
//     Non-empty Name fetches that specific credential (broker checks
//     allowed_aspects).
//   - Returned bundles are kind-specific JSON objects; see JiraBundle /
//     IMAPBundle / ProviderBundle for the parsed shapes.
//
// Out of scope: caching, refresh, multi-credential fetch in one call.
// V1 callers fetch once at startup and live with the returned bundle
// for the process lifetime; if the credential rotates, restart the MCP.
package brokercreds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/runtime/wsclient"
)

// JiraBundle mirrors credentials.JiraBundle (broker side). Kept as a
// local type so this package doesn't take a build-time dep on the
// nexus/credentials package — JSON-tag-compatible duplication, same
// rationale as keyfile's PersonalityBundle.
type JiraBundle struct {
	Email     string `json:"atlassian_email"`
	Token     string `json:"atlassian_token"`
	Subdomain string `json:"atlassian_subdomain"`
}

// IMAPBundle mirrors credentials.IMAPBundle (broker side).
type IMAPBundle struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	SSL      bool   `json:"ssl"`
}

// ProviderBundle mirrors credentials.ProviderBundle. Included for
// completeness; today's MCPs don't fetch provider creds via this path
// (the funnel's ProviderEnvResolver handles provider injection), but the
// frame supports it for callers that need the raw key for non-proxy
// code paths.
type ProviderBundle struct {
	APIShape     string `json:"api_shape"`
	BaseURL      string `json:"base_url"`
	Key          string `json:"key"`
	DefaultModel string `json:"default_model,omitempty"`
}

// ErrBrokerRejected is returned when the broker responds with a
// credential.fetch.error frame. The wrapped message is the broker's
// error string (e.g. "no default credential configured for aspect for
// kind=jira", "credential not found: prod-jira"). Callers surface this
// to the operator with enough context to act (rotate, re-configure
// allowed_aspects, etc.).
var ErrBrokerRejected = errors.New("brokercreds: broker rejected fetch")

// ErrUnexpectedKind is returned when the broker's result frame doesn't
// match the kind we asked for. Should never happen with a well-behaved
// broker; defensive check so a server bug surfaces as a clear error
// rather than a corrupt-bundle decode failure downstream.
var ErrUnexpectedKind = errors.New("brokercreds: broker returned unexpected kind")

// Fetch issues a credential.fetch frame and returns the parsed result.
// kind is required (one of "jira" | "imap" | "provider"). name is
// optional: empty → broker resolves the aspect's default-for-kind;
// non-empty → broker fetches that specific credential.
//
// Returns the raw map bundle plus the credential name the broker
// resolved. Callers who want a typed bundle use FetchJira / FetchIMAP /
// FetchProvider which call Fetch and json-round-trip the map into the
// concrete struct.
//
// ctx bounds the request — wsclient.Request handles disconnect and
// in-flight cancellation. A 15-second timeout at the call site is a
// reasonable upper bound for any sane broker round-trip; this package
// doesn't impose its own.
func Fetch(ctx context.Context, ws *wsclient.Client, kind, name string) (resolvedName string, bundle map[string]any, err error) {
	if ws == nil {
		return "", nil, errors.New("brokercreds.Fetch: ws client is nil")
	}
	if kind == "" {
		return "", nil, errors.New("brokercreds.Fetch: kind is required")
	}
	env, err := frames.NewRequest(frames.KindCredentialFetch, frames.CredentialFetchPayload{
		Kind: kind,
		Name: name,
	})
	if err != nil {
		return "", nil, fmt.Errorf("brokercreds.Fetch: encode: %w", err)
	}
	resp, err := ws.Request(ctx, env)
	if err != nil {
		return "", nil, fmt.Errorf("brokercreds.Fetch: WS request: %w", err)
	}

	// Error envelope shape: kind="credential.fetch.error", payload={"error": "..."}.
	if string(resp.Kind) == string(frames.KindCredentialFetch)+".error" {
		var errPayload struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(resp.Payload, &errPayload)
		if errPayload.Error == "" {
			errPayload.Error = "broker returned credential.fetch.error with no message"
		}
		return "", nil, fmt.Errorf("%w: %s", ErrBrokerRejected, errPayload.Error)
	}

	if resp.Kind != frames.KindCredentialFetchResult {
		return "", nil, fmt.Errorf("%w: got %q, want %q",
			ErrUnexpectedKind, resp.Kind, frames.KindCredentialFetchResult)
	}

	var result frames.CredentialFetchResultPayload
	if err := json.Unmarshal(resp.Payload, &result); err != nil {
		return "", nil, fmt.Errorf("brokercreds.Fetch: decode result: %w", err)
	}
	if result.Kind != kind {
		// Broker should never return a different kind than asked; this
		// is defence-in-depth in case a future server bug crosses wires.
		return "", nil, fmt.Errorf("%w: requested %q, broker returned %q",
			ErrUnexpectedKind, kind, result.Kind)
	}
	if result.Bundle == nil {
		return "", nil, errors.New("brokercreds.Fetch: broker returned empty bundle")
	}
	return result.Name, result.Bundle, nil
}

// FetchJira fetches a kind="jira" credential and decodes the bundle
// into a JiraBundle. Sugar over Fetch.
func FetchJira(ctx context.Context, ws *wsclient.Client, name string) (resolvedName string, bundle JiraBundle, err error) {
	resolvedName, raw, err := Fetch(ctx, ws, "jira", name)
	if err != nil {
		return "", JiraBundle{}, err
	}
	if err := remap(raw, &bundle); err != nil {
		return "", JiraBundle{}, fmt.Errorf("brokercreds.FetchJira: %w", err)
	}
	if bundle.Email == "" || bundle.Token == "" || bundle.Subdomain == "" {
		return "", JiraBundle{}, fmt.Errorf("brokercreds.FetchJira: broker returned incomplete bundle (email=%t token=%t subdomain=%t)",
			bundle.Email != "", bundle.Token != "", bundle.Subdomain != "")
	}
	return resolvedName, bundle, nil
}

// FetchIMAP fetches a kind="imap" credential and decodes the bundle
// into an IMAPBundle. Sugar over Fetch.
func FetchIMAP(ctx context.Context, ws *wsclient.Client, name string) (resolvedName string, bundle IMAPBundle, err error) {
	resolvedName, raw, err := Fetch(ctx, ws, "imap", name)
	if err != nil {
		return "", IMAPBundle{}, err
	}
	if err := remap(raw, &bundle); err != nil {
		return "", IMAPBundle{}, fmt.Errorf("brokercreds.FetchIMAP: %w", err)
	}
	if bundle.Host == "" || bundle.User == "" || bundle.Password == "" {
		return "", IMAPBundle{}, fmt.Errorf("brokercreds.FetchIMAP: broker returned incomplete bundle (host=%t user=%t password=%t)",
			bundle.Host != "", bundle.User != "", bundle.Password != "")
	}
	return resolvedName, bundle, nil
}

// FetchProvider fetches a kind="provider" credential. Per NEX-77's
// handler, provider fetches require a non-empty name (broker can't
// resolve a default without the api_shape context). Returns
// ErrBrokerRejected with the broker's diagnostic if name is empty.
func FetchProvider(ctx context.Context, ws *wsclient.Client, name string) (resolvedName string, bundle ProviderBundle, err error) {
	resolvedName, raw, err := Fetch(ctx, ws, "provider", name)
	if err != nil {
		return "", ProviderBundle{}, err
	}
	if err := remap(raw, &bundle); err != nil {
		return "", ProviderBundle{}, fmt.Errorf("brokercreds.FetchProvider: %w", err)
	}
	if bundle.Key == "" {
		return "", ProviderBundle{}, errors.New("brokercreds.FetchProvider: broker returned bundle with empty key")
	}
	return resolvedName, bundle, nil
}

// remap round-trips a map[string]any through JSON into a typed struct.
// The frame already decoded the bundle as map[string]any (Bundle's
// declared type in CredentialFetchResultPayload); re-marshaling and
// unmarshalling is the simplest way to honour the JSON tags on the
// typed bundle shapes. Not hot-path code (called once per MCP start).
func remap(src map[string]any, dst any) error {
	raw, err := json.Marshal(src)
	if err != nil {
		return fmt.Errorf("re-marshal bundle: %w", err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("decode bundle: %w", err)
	}
	return nil
}
