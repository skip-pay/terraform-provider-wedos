// Package client is a minimal client for the WEDOS HTTP REST API v2.
//
// The API answers every call with HTTP 200 and a JSON envelope
// {code, msg, request_id, data}. The real status is the `code` field, which the
// server sends as a string even though the documentation shows numbers.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultBaseURL = "https://api.wedos.com"
	DefaultScope   = "wdns"

	CodeOK      = 1000
	CodePending = 1001
)

// Config holds the client settings. Either AccessToken, or ClientID and
// ClientSecret must be set.
type Config struct {
	BaseURL      string
	ClientID     string
	ClientSecret string
	AccessToken  string
	Scope        string
	UserAgent    string
	HTTPClient   *http.Client
	// MaxRetries applies to read requests only. Writes are never retried on
	// ambiguous errors, because the API has no idempotency key.
	MaxRetries int
	RetryDelay time.Duration
}

type Client struct {
	cfg  Config
	http *http.Client

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if err := checkBaseURL(cfg.BaseURL); err != nil {
		return nil, err
	}
	if cfg.Scope == "" {
		cfg.Scope = DefaultScope
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "terraform-provider-wedos"
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	if cfg.RetryDelay == 0 {
		cfg.RetryDelay = 2 * time.Second
	}
	if cfg.AccessToken == "" && (cfg.ClientID == "" || cfg.ClientSecret == "") {
		return nil, errors.New("either access_token, or client_id and client_secret must be set")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{
			Timeout: 60 * time.Second,
			// The API never redirects. Following a redirect could resend the
			// Authorization header to another scheme or host; a 30x response
			// fails as an unexpected HTTP status instead.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &Client{cfg: cfg, http: hc}, nil
}

// checkBaseURL requires HTTPS, so credentials are never sent in clear text.
// Plain HTTP is accepted only for loopback addresses, where the test fake runs.
func checkBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid base_url %q", raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		if host == "localhost" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return fmt.Errorf("base_url %q must use https (plain http is allowed only for loopback addresses)", raw)
}

// APIError is a non-1000 result code returned in the response envelope.
type APIError struct {
	Code      int
	Msg       string
	RequestID string
	Method    string
	Path      string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("WEDOS API %s %s: code %d: %s (request_id %s)", e.Method, e.Path, e.Code, e.Msg, e.RequestID)
}

// IsAuth reports a missing, invalid or expired token. 3002 is not documented
// but is what the API returns for an invalid bearer token.
func (e *APIError) IsAuth() bool { return e.Code == 2000 || e.Code == 3002 }

// IsNotFound reports that the addressed domain or row does not exist.
// 3222: domain cannot be opened, 3306: domain deleted, 3309: row not found.
func (e *APIError) IsNotFound() bool { return e.Code == 3222 || e.Code == 3306 || e.Code == 3309 }

// IsTransient reports codes worth retrying: 4xxx external/communication
// errors (except the REST layer's 4000/4001/4005, which are request errors)
// and 6000 REST-to-WAPI initialisation errors.
func (e *APIError) IsTransient() bool {
	switch e.Code {
	case 4000, 4001, 4005:
		return false
	}
	return (e.Code >= 4000 && e.Code < 5000) || e.Code == 6000
}

// IsNotFound is a convenience wrapper over errors.As.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.IsNotFound()
}

// HasCode reports whether err is an APIError with the given code.
func HasCode(err error, code int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Code == code
}

type envelope struct {
	Code      FlexInt         `json:"code"`
	Msg       string          `json:"msg"`
	RequestID string          `json:"request_id"`
	Data      json.RawMessage `json:"data"`
}

// FlexInt decodes a JSON number or a numeric string. The API is inconsistent
// about which one it sends.
type FlexInt int64

func (f *FlexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("cannot decode %s as integer: %w", string(b), err)
	}
	*f = FlexInt(n)
	return nil
}

// FlexStrings decodes a JSON array of strings, or a string of addresses
// separated by spaces or commas — the documentation shows the string form,
// the live API returns the array.
type FlexStrings []string

func (f *FlexStrings) UnmarshalJSON(b []byte) error {
	var list []string
	if err := json.Unmarshal(b, &list); err == nil {
		*f = list
		return nil
	}
	var s *string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("cannot decode %s as string list: %w", string(b), err)
	}
	if s == nil {
		*f = nil
		return nil
	}
	*f = strings.FieldsFunc(*s, func(r rune) bool { return r == ' ' || r == ',' })
	return nil
}

func (c *Client) bearer(ctx context.Context, forceRefresh bool) (string, error) {
	if c.cfg.AccessToken != "" {
		return c.cfg.AccessToken, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !forceRefresh && c.token != "" && time.Now().Before(c.tokenExpiry) {
		return c.token, nil
	}

	form := url.Values{"grant_type": {"client_credentials"}, "scope": {c.cfg.Scope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.cfg.ClientID, c.cfg.ClientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", c.cfg.UserAgent)

	env, err := c.roundTrip(req)
	if err != nil {
		return "", err
	}
	if env.Code != CodeOK {
		// The OAuth error detail sits in data: {status_code, error, error_description}.
		var detail struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(env.Data, &detail)
		msg := env.Msg
		if detail.Error != "" {
			msg = fmt.Sprintf("%s: %s (%s)", msg, detail.Error, detail.Description)
		}
		return "", &APIError{Code: int(env.Code), Msg: msg, RequestID: env.RequestID, Method: http.MethodPost, Path: "/oauth2/token"}
	}
	var tok struct {
		AccessToken string  `json:"access_token"`
		ExpiresIn   FlexInt `json:"expires_in"`
	}
	if err := json.Unmarshal(env.Data, &tok); err != nil {
		return "", fmt.Errorf("decoding token response: %w", err)
	}
	if tok.AccessToken == "" {
		return "", errors.New("token response contains no access_token")
	}
	lifetime := time.Duration(tok.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = time.Hour
	}
	c.token = tok.AccessToken
	// Refresh a minute early so a request never leaves with a token about to expire.
	c.tokenExpiry = time.Now().Add(lifetime - time.Minute)
	return c.token, nil
}

func (c *Client) roundTrip(req *http.Request) (*envelope, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("WEDOS API %s %s: unexpected HTTP status %d", req.Method, req.URL.Path, resp.StatusCode)
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("WEDOS API %s %s: decoding response: %w", req.Method, req.URL.Path, err)
	}
	return &env, nil
}

// do sends one API call and decodes data into out (when out is non-nil).
// The path is relative to the base URL, e.g. /v2/dns/domains.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return err
		}
	}

	attempts := 1
	if method == http.MethodGet {
		attempts = c.cfg.MaxRetries + 1
	}
	refreshed := false

	for attempt := 1; ; attempt++ {
		token, err := c.bearer(ctx, false)
		if err != nil {
			return err
		}
		u := c.cfg.BaseURL + path
		if len(query) > 0 {
			u += "?" + query.Encode()
		}
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, body)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", c.cfg.UserAgent)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		env, err := c.roundTrip(req)
		if err != nil {
			if method == http.MethodGet && attempt < attempts {
				if werr := sleep(ctx, c.backoff(attempt)); werr != nil {
					return werr
				}
				continue
			}
			return err
		}

		if env.Code == CodeOK || env.Code == CodePending {
			if out != nil && len(env.Data) > 0 && string(env.Data) != "null" {
				if err := json.Unmarshal(env.Data, out); err != nil {
					return fmt.Errorf("WEDOS API %s %s: decoding data: %w", method, path, err)
				}
			}
			return nil
		}

		apiErr := &APIError{Code: int(env.Code), Msg: env.Msg, RequestID: env.RequestID, Method: method, Path: path}

		// An auth failure means the request was not executed, so one retry
		// with a fresh token is safe for writes too.
		if apiErr.IsAuth() && !refreshed && c.cfg.AccessToken == "" {
			refreshed = true
			if _, err := c.bearer(ctx, true); err != nil {
				return err
			}
			attempt--
			continue
		}
		if apiErr.IsTransient() && attempt < attempts {
			if werr := sleep(ctx, c.backoff(attempt)); werr != nil {
				return werr
			}
			continue
		}
		return apiErr
	}
}

func (c *Client) backoff(attempt int) time.Duration {
	return c.cfg.RetryDelay * time.Duration(1<<(attempt-1))
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
