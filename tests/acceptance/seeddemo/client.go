package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"
)

// client is a session against the running product API. The seed builder
// holds no database write path at all: every object, relation, evidence
// assertion, pull request, release, asset and fork in the demo is created by
// an HTTP call a human could have made from a browser. The build report
// records that path item by item.
type client struct {
	base string
	http *http.Client
	csrf string
	user string
}

func newClient(base string) (*client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &client{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Jar: jar, Timeout: 60 * time.Second},
	}, nil
}

// apiError is the product's error envelope plus the status line, so a caller
// can branch on the code and a human can read the message.
type apiError struct {
	Status  int
	Code    string `json:"code"`
	Message string `json:"message"`
	Body    string `json:"-"`
}

func (e *apiError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("HTTP %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

func (e *apiError) is(code string) bool { return e.Code == code }

// do performs one request. body may be nil; out may be nil. The Origin header
// is sent on every call because the pre-auth writes (signup, login) require
// it, and a request that carries it throughout is one fewer thing to get
// wrong on the retry path.
func (c *client) do(method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", c.base)
	if c.csrf != "" {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	if key := idempotencyKey(path); key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode >= 400 {
		ae := &apiError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
		_ = json.Unmarshal(raw, ae)
		return ae
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s %s: decode %s: %w", method, path, truncate(string(raw), 200), err)
		}
	}
	return nil
}

// idempotencyKey derives a stable key from the route, so a second run of the
// builder replays the product's own idempotency path instead of colliding.
// Only the routes that document an Idempotency-Key get one; sending it
// elsewhere would be a header the server ignores.
func idempotencyKey(path string) string {
	switch {
	case strings.HasSuffix(path, ":merge"):
		return "seed-demo" + path
	case strings.HasSuffix(path, ":request-review"):
		return "seed-demo" + path
	case strings.HasSuffix(path, "/pull-requests"):
		return "" // keyed explicitly by the caller: it names the PR's plan key
	default:
		return ""
	}
}

// send performs a request the caller built itself. It exists for the routes
// whose Idempotency-Key has to be derived from something do() cannot see — the
// PR-create route, where the key names the plan's own PR key so a re-run
// replays the pull request it already opened.
func (c *client) send(req *http.Request, out any) error {
	req.Header.Set("Accept", "application/json")
	if c.csrf != "" && req.Header.Get("X-CSRF-Token") == "" {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s %s: read body: %w", req.Method, req.URL.Path, err)
	}
	if resp.StatusCode >= 400 {
		ae := &apiError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
		_ = json.Unmarshal(raw, ae)
		return ae
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s %s: decode %s: %w", req.Method, req.URL.Path, truncate(string(raw), 200), err)
		}
	}
	return nil
}

// get is the read half. Reads need no CSRF token and (for public projects)
// no session either.
func (c *client) get(path string, out any) error { return c.do(http.MethodGet, path, nil, out) }

// post is the write half.
func (c *client) post(path string, body any, out any) error {
	return c.do(http.MethodPost, path, body, out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// login authenticates the client as the plan user, signing up first when the
// account does not exist. Both are product routes; nothing here writes a
// user row directly. A repeat run finds the account and logs in — that is
// the first half of "the second run must not fail on what the first left".
// login resolves one plan user's session, and it is LOGIN FIRST.
//
// A seed has to run twice: the second run finds the accounts the first one
// created. The account-creating route is not a way to ask whether an account
// exists — a duplicate signup is a REFUSAL there, and its refusal is not
// always one a caller can act on (see the note in cmdBuild's report and the
// follow-up issue: a signup whose email AND handle both collide answers 503
// SERVICE_UNAVAILABLE, not 409, because the handle-collision retry path in
// internal/persistence/auth_credential_store.go returns its second unique
// violation untranslated). Asking to log in and creating the account only
// when the credentials are unknown is the route contract's own shape: 401
// AUTH_INVALID_CREDENTIALS means "no such account or wrong password", which
// on a fresh database is the expected first answer.
//
// If the login is refused but the signup then reports the email as taken, the
// two together are an honest error: the account exists and this plan's
// password is not the one it was created with. That is an operator problem
// (a database seeded with a different plan), and it is reported as one rather
// than retried.
func (c *client) login(u PlanUser) (string, error) {
	var session struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		CSRFToken string `json:"csrf_token"`
	}
	err := c.post("/api/v1/auth/login", map[string]any{
		"email":    u.Email,
		"password": u.Password,
	}, &session)
	if err == nil {
		c.csrf = session.CSRFToken
		c.user = session.User.ID
		return session.User.ID, nil
	}
	var ae *apiError
	if !asAPIError(err, &ae) || ae.Status != http.StatusUnauthorized {
		return "", fmt.Errorf("login %s: %w", u.Email, err)
	}
	err = c.post("/api/v1/auth/signup", map[string]any{
		"email":        u.Email,
		"password":     u.Password,
		"handle":       u.Handle,
		"display_name": u.DisplayName,
	}, &session)
	if err != nil {
		return "", fmt.Errorf("signup %s (the login was refused as invalid credentials, and the signup was refused too): %w", u.Email, err)
	}
	c.csrf = session.CSRFToken
	c.user = session.User.ID
	return session.User.ID, nil
}

func asAPIError(err error, out **apiError) bool {
	for err != nil {
		if ae, ok := err.(*apiError); ok {
			*out = ae
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
