// Command smoke-register exercises the broker's registration endpoints
// end-to-end: register, heartbeat, list, deregister. Used for smoke-testing
// spec §6.2 before any real aspect exists.
//
// Run: go run ./scripts/smoke-register -nexus http://127.0.0.1:7888 -token testtoken
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/nexus-cw/nexus/shared/schemas"
)

func main() {
	nexusURL := flag.String("nexus", "http://127.0.0.1:7888", "broker base URL")
	token := flag.String("token", "", "bearer token (must match broker's NEXUS_TOKEN)")
	name := flag.String("name", "smoketest", "aspect name to register as")
	port := flag.Int("port", 7999, "fake port to claim")
	flag.Parse()

	if *token == "" {
		fmt.Fprintln(os.Stderr, "--token required")
		os.Exit(2)
	}

	c := &client{base: *nexusURL, token: *token}

	fmt.Println("1. GET /health (unauthenticated)")
	if err := c.health(); err != nil {
		die(err)
	}

	fmt.Println("2. POST /aspects/register")
	sessionID := fmt.Sprintf("smoke-%d", time.Now().UnixNano())
	if err := c.register(*name, *port, sessionID); err != nil {
		die(err)
	}

	fmt.Println("3. GET /aspects (should contain our entry)")
	if err := c.list(*name); err != nil {
		die(err)
	}

	fmt.Println("4. POST /aspects/heartbeat")
	if err := c.heartbeat(*name, sessionID); err != nil {
		die(err)
	}

	fmt.Println("5. POST /aspects/heartbeat with wrong session (expect 409)")
	if err := c.heartbeatExpectConflict(*name, "bogus-session"); err != nil {
		die(err)
	}

	fmt.Println("6. POST /aspects/deregister")
	if err := c.deregister(*name, sessionID); err != nil {
		die(err)
	}

	fmt.Println("7. GET /aspects (should no longer contain our entry)")
	if err := c.listAbsent(*name); err != nil {
		die(err)
	}

	fmt.Println("\nALL CHECKS PASSED")
}

type client struct {
	base, token string
}

func (c *client) do(method, path string, body any, auth bool) ([]byte, int, error) {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reqBody = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, c.base+path, reqBody)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	return bodyBytes, resp.StatusCode, err
}

func (c *client) health() error {
	body, status, err := c.do("GET", "/health", nil, false)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("health: got %d, body %s", status, body)
	}
	return nil
}

func (c *client) register(name string, port int, sessionID string) error {
	req := schemas.RegisterRequest{
		Name:         name,
		ContextMode:  schemas.ContextStateless,
		Provider:     "claude-api",
		Port:         port,
		PID:          os.Getpid(),
		StartedAt:    time.Now().UTC(),
		Capabilities: []string{"smoke"},
		Home:         "(smoke test)",
		SessionID:    sessionID,
	}
	body, status, err := c.do("POST", "/aspects/register", req, true)
	if err != nil {
		return err
	}
	if status != 201 {
		return fmt.Errorf("register: got %d, body %s", status, body)
	}
	var resp schemas.RegisterResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return err
	}
	if resp.Status != "registered" {
		return fmt.Errorf("register: unexpected status %q", resp.Status)
	}
	return nil
}

func (c *client) heartbeat(name, sessionID string) error {
	req := schemas.HeartbeatRequest{Name: name, SessionID: sessionID, At: time.Now().UTC()}
	body, status, err := c.do("POST", "/aspects/heartbeat", req, true)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("heartbeat: got %d, body %s", status, body)
	}
	return nil
}

func (c *client) heartbeatExpectConflict(name, bogusSession string) error {
	req := schemas.HeartbeatRequest{Name: name, SessionID: bogusSession, At: time.Now().UTC()}
	_, status, err := c.do("POST", "/aspects/heartbeat", req, true)
	if err != nil {
		return err
	}
	if status != 409 {
		return fmt.Errorf("heartbeat-wrong-session: expected 409, got %d", status)
	}
	return nil
}

func (c *client) deregister(name, sessionID string) error {
	req := schemas.DeregisterRequest{Name: name, SessionID: sessionID, Reason: "smoke test done"}
	body, status, err := c.do("POST", "/aspects/deregister", req, true)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("deregister: got %d, body %s", status, body)
	}
	return nil
}

func (c *client) list(expectedName string) error {
	body, status, err := c.do("GET", "/aspects", nil, true)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("list: got %d, body %s", status, body)
	}
	var resp struct {
		Aspects []schemas.AspectState `json:"aspects"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return err
	}
	for _, a := range resp.Aspects {
		if a.Name == expectedName {
			return nil
		}
	}
	return fmt.Errorf("list: expected to find %q in roster", expectedName)
}

func (c *client) listAbsent(name string) error {
	body, status, err := c.do("GET", "/aspects", nil, true)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("list: got %d, body %s", status, body)
	}
	var resp struct {
		Aspects []schemas.AspectState `json:"aspects"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return err
	}
	for _, a := range resp.Aspects {
		if a.Name == name {
			return fmt.Errorf("list: %q unexpectedly still in roster after deregister", name)
		}
	}
	return nil
}

func die(err error) {
	fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
	os.Exit(1)
}
