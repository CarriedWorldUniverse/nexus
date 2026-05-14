// IMAP client wrapper for nexus-imap-mcp. Holds one long-lived
// connection, re-establishes on failure, exposes a small API the MCP
// tool handlers call into.
//
// Library: github.com/emersion/go-imap/v2.

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// MessageRef is the lightweight projection of a message returned by
// list-shaped tools. UID is stable for the mailbox lifetime; other
// tools take it as the primary handle.
type MessageRef struct {
	UID     uint32 `json:"uid"`
	From    string `json:"from"`
	Subject string `json:"subject"`
	Date    string `json:"date"`               // RFC 3339, server-stamped at receive
	Body    string `json:"body,omitempty"`     // plain-text excerpt (first 8 KiB)
	Folder  string `json:"folder,omitempty"`   // populated when crossing folders
}

// Client wraps an imapclient.Client with a mutex so multiple MCP tool
// calls don't race the single connection.
type Client struct {
	host     string
	port     int
	username string
	password string

	mu  sync.Mutex
	c   *imapclient.Client
	sel string // currently-selected mailbox name (empty = none)
}

// NewClient builds a configured client. Connection is lazy — call
// Probe (or any tool) to open the socket. defaultPort=993 (IMAP+TLS)
// when port is 0.
func NewClient(host string, port int, username, password string) *Client {
	if port == 0 {
		port = 993
	}
	return &Client{host: host, port: port, username: username, password: password}
}

// Probe opens the connection, logs in, and selects INBOX. Used at
// startup as a credential smoke test before announcing ready.
func (c *Client) Probe(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.connectLocked(); err != nil {
		return err
	}
	if _, err := c.c.Select("INBOX", nil).Wait(); err != nil {
		return fmt.Errorf("imap: SELECT INBOX: %w", err)
	}
	c.sel = "INBOX"
	return nil
}

// connectLocked dials + LOGIN. Caller holds c.mu.
func (c *Client) connectLocked() error {
	if c.c != nil {
		return nil
	}
	addr := fmt.Sprintf("%s:%d", c.host, c.port)
	cl, err := imapclient.DialTLS(addr, &imapclient.Options{
		TLSConfig: &tls.Config{ServerName: c.host},
	})
	if err != nil {
		return fmt.Errorf("imap: DialTLS %s: %w", addr, err)
	}
	if err := cl.Login(c.username, c.password).Wait(); err != nil {
		_ = cl.Close()
		return fmt.Errorf("imap: LOGIN: %w", err)
	}
	c.c = cl
	c.sel = ""
	return nil
}

// reconnectLocked drops + reopens. Used when a command returns an
// error that looks like a dead connection.
func (c *Client) reconnectLocked() error {
	if c.c != nil {
		_ = c.c.Close()
		c.c = nil
		c.sel = ""
	}
	return c.connectLocked()
}

// withSelected ensures the named mailbox is selected, calling the
// passed function with the live client. Caller holds c.mu.
func (c *Client) withSelectedLocked(folder string, fn func(*imapclient.Client) error) error {
	if err := c.connectLocked(); err != nil {
		return err
	}
	if c.sel != folder {
		if _, err := c.c.Select(folder, nil).Wait(); err != nil {
			// Retry once after a forced reconnect — long-idle connections
			// silently die and report a generic protocol error on the
			// first command after.
			if err2 := c.reconnectLocked(); err2 != nil {
				return fmt.Errorf("imap: reconnect: %w (after %v)", err2, err)
			}
			if _, err3 := c.c.Select(folder, nil).Wait(); err3 != nil {
				return fmt.Errorf("imap: SELECT %s: %w", folder, err3)
			}
		}
		c.sel = folder
	}
	return fn(c.c)
}

// ListFolders returns every mailbox visible to the authenticated user.
func (c *Client) ListFolders(ctx context.Context) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.connectLocked(); err != nil {
		return nil, err
	}
	cmd := c.c.List("", "*", nil)
	var out []string
	for {
		mb := cmd.Next()
		if mb == nil {
			break
		}
		out = append(out, mb.Mailbox)
	}
	if err := cmd.Close(); err != nil {
		return nil, fmt.Errorf("imap: LIST: %w", err)
	}
	return out, nil
}

// FetchRecent returns up to `limit` recent messages from `folder`,
// optionally filtered by from/subject substring + since date. Newest
// first. Body is included as a best-effort plain-text excerpt (first
// 8 KiB).
func (c *Client) FetchRecent(ctx context.Context, folder string, limit int, fromFilter, subjectFilter string, since time.Time) ([]MessageRef, error) {
	if limit <= 0 {
		limit = 20
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []MessageRef
	err := c.withSelectedLocked(folder, func(cl *imapclient.Client) error {
		// Server-side filter via SEARCH for the cheap criteria; client-
		// side substring filter for case-insensitive contains semantics.
		criteria := &imap.SearchCriteria{}
		if !since.IsZero() {
			criteria.Since = since
		}
		if fromFilter != "" {
			criteria.Header = append(criteria.Header, imap.SearchCriteriaHeaderField{Key: "From", Value: fromFilter})
		}
		if subjectFilter != "" {
			criteria.Header = append(criteria.Header, imap.SearchCriteriaHeaderField{Key: "Subject", Value: subjectFilter})
		}
		// UIDSearch (not Search) so AllUIDs() returns populated UID
		// list; plain Search returns MSN-keyed results that don't line
		// up with the UID-set FETCH we issue below.
		searchData, err := cl.UIDSearch(criteria, nil).Wait()
		if err != nil {
			return fmt.Errorf("SEARCH: %w", err)
		}
		uids := searchData.AllUIDs()
		// Server returns oldest-first; we want newest-first capped to limit.
		if len(uids) > limit {
			uids = uids[len(uids)-limit:]
		}
		for i, j := 0, len(uids)-1; i < j; i, j = i+1, j-1 {
			uids[i], uids[j] = uids[j], uids[i]
		}
		if len(uids) == 0 {
			return nil
		}
		set := imap.UIDSetNum(uids...)
		fetchOpts := &imap.FetchOptions{
			Envelope: true,
			UID:      true,
			BodySection: []*imap.FetchItemBodySection{
				{Specifier: imap.PartSpecifierText, Peek: true, Partial: &imap.SectionPartial{Offset: 0, Size: 8192}},
			},
		}
		msgs := cl.Fetch(set, fetchOpts)
		for {
			msg := msgs.Next()
			if msg == nil {
				break
			}
			ref, err := messageToRef(msg)
			if err != nil {
				return err
			}
			ref.Folder = folder
			out = append(out, ref)
		}
		return msgs.Close()
	})
	return out, err
}

// FetchOTP scans `folder` for a recent message from `sender` (substring
// match, case-insensitive) within the last `maxAge`, and returns the
// first 6-digit numeric code found in the subject or body. Empty
// sender means "any sender." Returns ErrOTPNotFound when nothing
// matches.
var ErrOTPNotFound = errors.New("imap: no OTP code found")

var otpRE = regexp.MustCompile(`\b\d{6}\b`)

func (c *Client) FetchOTP(ctx context.Context, folder, sender string, maxAge time.Duration) (string, MessageRef, error) {
	since := time.Now().Add(-maxAge)
	msgs, err := c.FetchRecent(ctx, folder, 20, sender, "", since)
	if err != nil {
		return "", MessageRef{}, err
	}
	for _, m := range msgs {
		// Check subject first — many providers put the code there.
		if code := otpRE.FindString(m.Subject); code != "" {
			return code, m, nil
		}
		if code := otpRE.FindString(m.Body); code != "" {
			return code, m, nil
		}
	}
	return "", MessageRef{}, ErrOTPNotFound
}

// Move relocates a message UID from src → dst folder. Auto-creates
// dst if it doesn't exist (some servers reject MOVE to non-existent
// folders).
func (c *Client) Move(ctx context.Context, src string, uid uint32, dst string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.connectLocked(); err != nil {
		return err
	}
	// Ensure dst exists — CREATE on an existing folder is a no-op on
	// most servers, but those that error get the existence check below.
	cmd := c.c.List("", dst, nil)
	exists := false
	for {
		mb := cmd.Next()
		if mb == nil {
			break
		}
		if mb.Mailbox == dst {
			exists = true
		}
	}
	if err := cmd.Close(); err != nil {
		return fmt.Errorf("LIST %s: %w", dst, err)
	}
	if !exists {
		if err := c.c.Create(dst, nil).Wait(); err != nil {
			return fmt.Errorf("CREATE %s: %w", dst, err)
		}
	}
	return c.withSelectedLocked(src, func(cl *imapclient.Client) error {
		_, err := cl.Move(imap.UIDSetNum(imap.UID(uid)), dst).Wait()
		return err
	})
}

// Delete marks UID as \Deleted and expunges. One-shot — caller doesn't
// need to call Expunge separately.
func (c *Client) Delete(ctx context.Context, folder string, uid uint32) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.withSelectedLocked(folder, func(cl *imapclient.Client) error {
		store := &imap.StoreFlags{
			Op:     imap.StoreFlagsAdd,
			Silent: true,
			Flags:  []imap.Flag{imap.FlagDeleted},
		}
		if err := cl.Store(imap.UIDSetNum(imap.UID(uid)), store, nil).Close(); err != nil {
			return fmt.Errorf("STORE +Deleted: %w", err)
		}
		if _, err := cl.Expunge().Collect(); err != nil {
			return fmt.Errorf("EXPUNGE: %w", err)
		}
		return nil
	})
}

// Mark sets or clears flags on a message UID. add and remove can each
// hold multiple flags; nil/empty lists are fine.
func (c *Client) Mark(ctx context.Context, folder string, uid uint32, add, remove []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.withSelectedLocked(folder, func(cl *imapclient.Client) error {
		for op, flags := range map[imap.StoreFlagsOp][]string{
			imap.StoreFlagsAdd: add,
			imap.StoreFlagsDel: remove,
		} {
			if len(flags) == 0 {
				continue
			}
			imapFlags := make([]imap.Flag, 0, len(flags))
			for _, f := range flags {
				imapFlags = append(imapFlags, imap.Flag(f))
			}
			store := &imap.StoreFlags{Op: op, Silent: true, Flags: imapFlags}
			if err := cl.Store(imap.UIDSetNum(imap.UID(uid)), store, nil).Close(); err != nil {
				return fmt.Errorf("STORE %v: %w", op, err)
			}
		}
		return nil
	})
}

// messageToRef converts a go-imap/v2 FetchMessageData into our flatter
// MessageRef projection. Extracts the plain-text body if available,
// otherwise leaves it empty.
func messageToRef(msg *imapclient.FetchMessageData) (MessageRef, error) {
	ref := MessageRef{}
	for {
		item := msg.Next()
		if item == nil {
			break
		}
		switch v := item.(type) {
		case imapclient.FetchItemDataUID:
			ref.UID = uint32(v.UID)
		case imapclient.FetchItemDataEnvelope:
			if v.Envelope != nil {
				if len(v.Envelope.From) > 0 {
					ref.From = formatAddress(v.Envelope.From[0])
				}
				ref.Subject = v.Envelope.Subject
				if !v.Envelope.Date.IsZero() {
					ref.Date = v.Envelope.Date.UTC().Format(time.RFC3339)
				}
			}
		case imapclient.FetchItemDataBodySection:
			if v.Literal != nil {
				buf, err := io.ReadAll(v.Literal)
				if err == nil {
					ref.Body = string(buf)
				}
			}
		}
	}
	return ref, nil
}

func formatAddress(a imap.Address) string {
	mailbox := strings.TrimSpace(a.Mailbox)
	host := strings.TrimSpace(a.Host)
	if mailbox == "" && host == "" {
		return ""
	}
	addr := mailbox + "@" + host
	if a.Name != "" {
		return fmt.Sprintf("%s <%s>", a.Name, addr)
	}
	return addr
}
