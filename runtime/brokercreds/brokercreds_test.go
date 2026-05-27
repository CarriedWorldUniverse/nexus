package brokercreds

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/runtime/wsclient"
)

// fakeBroker accepts one WS connection and dispatches each incoming
// frame to onFrame. Mirrors the pattern in wsclient_test.go.
func fakeBroker(t *testing.T, onFrame func(*websocket.Conn, frames.Envelope)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsc, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			t.Logf("accept: %v", err)
			return
		}
		defer wsc.Close(websocket.StatusNormalClosure, "done")
		wsc.SetReadLimit(1 << 20)
		ctx := context.Background()
		for {
			_, data, err := wsc.Read(ctx)
			if err != nil {
				return
			}
			env, err := frames.Decode(data)
			if err != nil {
				continue
			}
			onFrame(wsc, env)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// startClient brings up a wsclient.Client against the fake broker and
// waits until it's connected. Returns the live client.
func startClient(t *testing.T, srv *httptest.Server) (*wsclient.Client, context.CancelFunc) {
	t.Helper()
	c, err := wsclient.New(wsclient.Config{
		URL:       wsURL(srv),
		AuthToken: "tok",
		Handler:   wsclient.HandlerFunc(func(frames.Envelope) {}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = c.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for !c.Connected() {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("wsclient did not connect within 2s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return c, cancel
}

// replyResult crafts the broker's success-path reply.
func replyResult(wsc *websocket.Conn, inReplyTo, kind, name string, bundle map[string]any) error {
	resp, err := frames.NewResponse(frames.KindCredentialFetchResult, inReplyTo, frames.CredentialFetchResultPayload{
		Name:   name,
		Kind:   kind,
		Bundle: bundle,
	})
	if err != nil {
		return err
	}
	raw, err := frames.Encode(resp)
	if err != nil {
		return err
	}
	return wsc.Write(context.Background(), websocket.MessageText, raw)
}

// replyError crafts a credential.fetch.error envelope.
func replyError(wsc *websocket.Conn, inReplyTo, msg string) error {
	resp, err := frames.NewResponse(frames.Kind(string(frames.KindCredentialFetch)+".error"), inReplyTo, map[string]string{"error": msg})
	if err != nil {
		return err
	}
	raw, err := frames.Encode(resp)
	if err != nil {
		return err
	}
	return wsc.Write(context.Background(), websocket.MessageText, raw)
}

func TestFetchJira_Default(t *testing.T) {
	srv := fakeBroker(t, func(wsc *websocket.Conn, env frames.Envelope) {
		if env.Kind != frames.KindCredentialFetch {
			return
		}
		var p frames.CredentialFetchPayload
		_ = frames.PayloadAs(env, &p)
		if p.Kind != "jira" || p.Name != "" {
			_ = replyError(wsc, env.ID, "unexpected payload")
			return
		}
		_ = replyResult(wsc, env.ID, "jira", "shadow-jira", map[string]any{
			"atlassian_email":     "shadow@example.com",
			"atlassian_token":     "secret-token",
			"atlassian_subdomain": "carriedworlduniverse",
		})
	})
	c, cancel := startClient(t, srv)
	defer cancel()

	ctx, ccancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer ccancel()
	name, b, err := FetchJira(ctx, c, "")
	if err != nil {
		t.Fatalf("FetchJira: %v", err)
	}
	if name != "shadow-jira" {
		t.Errorf("name = %q, want shadow-jira", name)
	}
	if b.Email != "shadow@example.com" || b.Token != "secret-token" || b.Subdomain != "carriedworlduniverse" {
		t.Errorf("bundle = %+v", b)
	}
}

// NEX-88: FetchJira surfaces ProjectKey when the broker includes it
// in the credential bundle. Lets aspects fetching a credential pick
// up the operator-curated default project without per-keyfile setup.
func TestFetchJira_WithProjectKey(t *testing.T) {
	srv := fakeBroker(t, func(wsc *websocket.Conn, env frames.Envelope) {
		if env.Kind != frames.KindCredentialFetch {
			return
		}
		var p frames.CredentialFetchPayload
		_ = frames.PayloadAs(env, &p)
		if p.Kind != "jira" {
			_ = replyError(wsc, env.ID, "unexpected kind")
			return
		}
		_ = replyResult(wsc, env.ID, "jira", "wks-jira", map[string]any{
			"atlassian_email":     "wren@example.com",
			"atlassian_token":     "secret-token",
			"atlassian_subdomain": "carriedworlduniverse",
			"project_key":         "WKS",
		})
	})
	c, cancel := startClient(t, srv)
	defer cancel()

	ctx, ccancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer ccancel()
	name, b, err := FetchJira(ctx, c, "wks-jira")
	if err != nil {
		t.Fatalf("FetchJira: %v", err)
	}
	if name != "wks-jira" {
		t.Errorf("name = %q, want wks-jira", name)
	}
	if b.ProjectKey != "WKS" {
		t.Errorf("ProjectKey = %q, want WKS (broker default project must flow to consumer)", b.ProjectKey)
	}
	if b.Email != "wren@example.com" {
		t.Errorf("Email round-trip broken: %q", b.Email)
	}
}

// NEX-88: a broker that omits project_key (legacy / not-yet-set)
// produces a JiraBundle with empty ProjectKey but other fields intact.
// Consumer's resolution chain then falls back to keyfile / per-call.
func TestFetchJira_WithoutProjectKey_BackCompat(t *testing.T) {
	srv := fakeBroker(t, func(wsc *websocket.Conn, env frames.Envelope) {
		if env.Kind != frames.KindCredentialFetch {
			return
		}
		_ = replyResult(wsc, env.ID, "jira", "legacy-jira", map[string]any{
			"atlassian_email":     "legacy@example.com",
			"atlassian_token":     "secret",
			"atlassian_subdomain": "carriedworlduniverse",
		})
	})
	c, cancel := startClient(t, srv)
	defer cancel()

	ctx, ccancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer ccancel()
	_, b, err := FetchJira(ctx, c, "legacy-jira")
	if err != nil {
		t.Fatalf("FetchJira: %v", err)
	}
	if b.ProjectKey != "" {
		t.Errorf("ProjectKey should be empty on a pre-NEX-88 bundle; got %q", b.ProjectKey)
	}
	if b.Email != "legacy@example.com" {
		t.Errorf("Email round-trip broken: %q", b.Email)
	}
}

func TestFetchIMAP_ByName(t *testing.T) {
	srv := fakeBroker(t, func(wsc *websocket.Conn, env frames.Envelope) {
		if env.Kind != frames.KindCredentialFetch {
			return
		}
		var p frames.CredentialFetchPayload
		_ = frames.PayloadAs(env, &p)
		if p.Kind != "imap" || p.Name != "shadow-mail" {
			_ = replyError(wsc, env.ID, "unexpected payload")
			return
		}
		_ = replyResult(wsc, env.ID, "imap", "shadow-mail", map[string]any{
			"host":     "mail.example.com",
			"port":     993,
			"user":     "shadow@example.com",
			"password": "hunter2",
			"ssl":      true,
		})
	})
	c, cancel := startClient(t, srv)
	defer cancel()

	ctx, ccancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer ccancel()
	name, b, err := FetchIMAP(ctx, c, "shadow-mail")
	if err != nil {
		t.Fatalf("FetchIMAP: %v", err)
	}
	if name != "shadow-mail" {
		t.Errorf("name = %q, want shadow-mail", name)
	}
	if b.Host != "mail.example.com" || b.Port != 993 || b.User != "shadow@example.com" || b.Password != "hunter2" || !b.SSL {
		t.Errorf("bundle = %+v", b)
	}
}

func TestFetch_BrokerError(t *testing.T) {
	srv := fakeBroker(t, func(wsc *websocket.Conn, env frames.Envelope) {
		if env.Kind != frames.KindCredentialFetch {
			return
		}
		_ = replyError(wsc, env.ID, "no default credential configured for aspect for kind=jira")
	})
	c, cancel := startClient(t, srv)
	defer cancel()

	ctx, ccancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer ccancel()
	_, _, err := FetchJira(ctx, c, "")
	if !errors.Is(err, ErrBrokerRejected) {
		t.Fatalf("err = %v, want ErrBrokerRejected", err)
	}
	if !strings.Contains(err.Error(), "no default credential configured") {
		t.Errorf("err message missing broker diagnostic: %v", err)
	}
}

func TestFetch_KindMismatch(t *testing.T) {
	srv := fakeBroker(t, func(wsc *websocket.Conn, env frames.Envelope) {
		if env.Kind != frames.KindCredentialFetch {
			return
		}
		// Lie about the kind in the response.
		_ = replyResult(wsc, env.ID, "imap", "wrong-kind", map[string]any{
			"host": "x", "port": 1, "user": "u", "password": "p", "ssl": true,
		})
	})
	c, cancel := startClient(t, srv)
	defer cancel()

	ctx, ccancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer ccancel()
	_, _, err := FetchJira(ctx, c, "")
	if !errors.Is(err, ErrUnexpectedKind) {
		t.Fatalf("err = %v, want ErrUnexpectedKind", err)
	}
}

func TestFetchJira_IncompleteBundle(t *testing.T) {
	srv := fakeBroker(t, func(wsc *websocket.Conn, env frames.Envelope) {
		if env.Kind != frames.KindCredentialFetch {
			return
		}
		_ = replyResult(wsc, env.ID, "jira", "broken", map[string]any{
			"atlassian_email": "x@example.com",
			// token + subdomain deliberately absent
		})
	})
	c, cancel := startClient(t, srv)
	defer cancel()

	ctx, ccancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer ccancel()
	_, _, err := FetchJira(ctx, c, "")
	if err == nil {
		t.Fatal("expected incomplete-bundle error, got nil")
	}
	if !strings.Contains(err.Error(), "incomplete bundle") {
		t.Errorf("err = %v, want incomplete-bundle diagnostic", err)
	}
}

// replyModelConfig crafts the broker's success-path reply for
// aspect.model_config.get. NEX-293 helper.
func replyModelConfig(wsc *websocket.Conn, inReplyTo string, p frames.AspectModelConfigGetResultPayload) error {
	resp, err := frames.NewResponse(frames.KindAspectModelConfigGetResult, inReplyTo, p)
	if err != nil {
		return err
	}
	raw, err := frames.Encode(resp)
	if err != nil {
		return err
	}
	return wsc.Write(context.Background(), websocket.MessageText, raw)
}

// NEX-293: round-trip a populated AspectModelConfig through the
// aspect.model_config.get frame.
func TestFetchAspectModelConfig_Populated(t *testing.T) {
	srv := fakeBroker(t, func(wsc *websocket.Conn, env frames.Envelope) {
		if env.Kind != frames.KindAspectModelConfigGet {
			return
		}
		_ = replyModelConfig(wsc, env.ID, frames.AspectModelConfigGetResultPayload{
			Aspect:          "anvil",
			JudgeModel:      "haiku",
			JudgeCredential: "anvil-judge-deepseek",
		})
	})
	cli, cancel := startClient(t, srv)
	defer cancel()

	ctx, cctx := context.WithTimeout(context.Background(), 2*time.Second)
	defer cctx()
	cfg, err := FetchAspectModelConfig(ctx, cli)
	if err != nil {
		t.Fatalf("FetchAspectModelConfig: %v", err)
	}
	if cfg.Aspect != "anvil" {
		t.Errorf("Aspect = %q, want anvil", cfg.Aspect)
	}
	if cfg.JudgeModel != "haiku" {
		t.Errorf("JudgeModel = %q, want haiku", cfg.JudgeModel)
	}
	if cfg.JudgeCredential != "anvil-judge-deepseek" {
		t.Errorf("JudgeCredential = %q, want anvil-judge-deepseek", cfg.JudgeCredential)
	}
}

// NEX-293: when no admin overrides are configured the broker returns
// an empty (all-empty-string) result. The fetcher must surface that
// cleanly, not as an error.
func TestFetchAspectModelConfig_Empty(t *testing.T) {
	srv := fakeBroker(t, func(wsc *websocket.Conn, env frames.Envelope) {
		if env.Kind != frames.KindAspectModelConfigGet {
			return
		}
		_ = replyModelConfig(wsc, env.ID, frames.AspectModelConfigGetResultPayload{
			Aspect: "anvil",
		})
	})
	cli, cancel := startClient(t, srv)
	defer cancel()

	ctx, cctx := context.WithTimeout(context.Background(), 2*time.Second)
	defer cctx()
	cfg, err := FetchAspectModelConfig(ctx, cli)
	if err != nil {
		t.Fatalf("FetchAspectModelConfig: %v", err)
	}
	if cfg.JudgeModel != "" || cfg.JudgeCredential != "" ||
		cfg.PrimaryModel != "" || cfg.PrimaryCredential != "" ||
		cfg.CompactModel != "" || cfg.CompactCredential != "" {
		t.Errorf("empty broker response should produce empty config; got %+v", cfg)
	}
}

// NEX-293: broker-side error response (e.g. credentials store not
// configured, identity not resolved) surfaces as ErrBrokerRejected
// with the broker's diagnostic string. Caller is expected to log +
// fall back to defaults, not crash.
func TestFetchAspectModelConfig_BrokerError(t *testing.T) {
	srv := fakeBroker(t, func(wsc *websocket.Conn, env frames.Envelope) {
		if env.Kind != frames.KindAspectModelConfigGet {
			return
		}
		errKind := frames.Kind(string(frames.KindAspectModelConfigGet) + ".error")
		resp, _ := frames.NewResponse(errKind, env.ID, map[string]string{"error": "no aspect identity"})
		raw, _ := frames.Encode(resp)
		_ = wsc.Write(context.Background(), websocket.MessageText, raw)
	})
	cli, cancel := startClient(t, srv)
	defer cancel()

	ctx, cctx := context.WithTimeout(context.Background(), 2*time.Second)
	defer cctx()
	_, err := FetchAspectModelConfig(ctx, cli)
	if err == nil {
		t.Fatal("expected error from broker .error envelope")
	}
	if !errors.Is(err, ErrBrokerRejected) {
		t.Errorf("err = %v, want wraps ErrBrokerRejected", err)
	}
	if !strings.Contains(err.Error(), "no aspect identity") {
		t.Errorf("err = %v, want broker diagnostic surfaced", err)
	}
}
