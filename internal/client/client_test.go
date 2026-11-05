package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skip-pay/terraform-provider-wedos/internal/wedosmock"
)

func TestFlexIntAcceptsStringAndNumber(t *testing.T) {
	for in, want := range map[string]int64{`"1000"`: 1000, `2000`: 2000, `null`: 0, `""`: 0} {
		var f FlexInt
		if err := json.Unmarshal([]byte(in), &f); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if int64(f) != want {
			t.Errorf("%s: got %d, want %d", in, f, want)
		}
	}
}

func newTestClient(t *testing.T, s *wedosmock.Server) *Client {
	t.Helper()
	c, err := New(Config{BaseURL: s.URL, ClientID: s.ClientID, ClientSecret: s.ClientSecret, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRowLifecycleAgainstMock(t *testing.T) {
	s := wedosmock.New()
	defer s.Close()
	c := newTestClient(t, s)
	ctx := t.Context()

	if err := c.AddDomain(ctx, DomainCreate{Name: "example.cz", Type: "primary"}); err != nil {
		t.Fatal(err)
	}
	id, err := c.AddRow(ctx, "example.cz", RowCreate{Name: "www", TTL: 300, Type: "A", Rdata: "192.0.2.1"})
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Fatal("AddRow returned no row_id")
	}
	if len(s.PublishedRows("example.cz")) != 0 {
		t.Fatal("row published before commit")
	}
	if err := c.Commit(ctx, "example.cz"); err != nil {
		t.Fatal(err)
	}
	if got := s.PublishedRows("example.cz"); len(got) != 1 || got[0].Rdata != "192.0.2.1" {
		t.Fatalf("published rows: %+v", got)
	}

	_, err = c.AddRow(ctx, "example.cz", RowCreate{Name: "www", TTL: 300, Type: "A", Rdata: "192.0.2.1"})
	if !HasCode(err, 2316) {
		t.Fatalf("duplicate row: want code 2316, got %v", err)
	}

	if err := c.UpdateRow(ctx, "example.cz", id, RowUpdate{TTL: 600, Rdata: "192.0.2.2"}); err != nil {
		t.Fatal(err)
	}
	rows, err := c.ListRows(ctx, "example.cz")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].TTL != 600 || rows[0].Rdata != "192.0.2.2" {
		t.Fatalf("rows after update: %+v", rows)
	}

	if err := c.DeleteRow(ctx, "example.cz", id); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteRow(ctx, "example.cz", id); !IsNotFound(err) {
		t.Fatalf("second delete: want not found, got %v", err)
	}
	if _, err := c.GetDomain(ctx, "missing.cz"); !IsNotFound(err) {
		t.Fatalf("missing domain: want not found, got %v", err)
	}
}

func TestTokenIsRefreshedAfterRevocation(t *testing.T) {
	s := wedosmock.New()
	defer s.Close()
	c := newTestClient(t, s)
	ctx := t.Context()

	if _, err := c.ListDomains(ctx); err != nil {
		t.Fatal(err)
	}
	s.RevokeTokens()
	// The cached token is now rejected with 3002; the client must fetch a new
	// one and repeat the call once, also for a write.
	if err := c.AddDomain(ctx, DomainCreate{Name: "example.cz"}); err != nil {
		t.Fatalf("write after revocation: %v", err)
	}
}

func TestInvalidClientSecret(t *testing.T) {
	s := wedosmock.New()
	defer s.Close()
	c, _ := New(Config{BaseURL: s.URL, ClientID: s.ClientID, ClientSecret: "wrong"})
	_, err := c.ListDomains(t.Context())
	if !HasCode(err, 2000) {
		t.Fatalf("want code 2000, got %v", err)
	}
}

func TestStaticAccessToken(t *testing.T) {
	s := wedosmock.New()
	defer s.Close()
	c, err := New(Config{BaseURL: s.URL, AccessToken: s.IssueToken()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListDomains(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestTransientErrorsRetryOnlyReads(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "4201", "msg": "registry connection error", "request_id": "x"})
	}))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, AccessToken: "t", MaxRetries: 2, RetryDelay: time.Millisecond})

	if _, err := c.ListRows(t.Context(), "example.cz"); !HasCode(err, 4201) {
		t.Fatalf("read: want 4201, got %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("read: want 3 attempts, got %d", got)
	}

	calls.Store(0)
	if _, err := c.AddRow(t.Context(), "example.cz", RowCreate{Name: "a", TTL: 1, Type: "A", Rdata: "192.0.2.1"}); !HasCode(err, 4201) {
		t.Fatalf("write: want 4201, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("write: want 1 attempt, got %d", got)
	}
}

func TestListDomainsFollowsPages(t *testing.T) {
	s := wedosmock.New()
	defer s.Close()
	for i := 0; i < 230; i++ {
		s.AddZone(string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".cz")
	}
	got, err := newTestClient(t, s).ListDomains(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 230 {
		t.Fatalf("want 230 domains, got %d", len(got))
	}
}

func TestBaseURLMustBeHTTPS(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://api.wedos.com": true,
		"http://127.0.0.1:8080": true,
		"http://localhost:8080": true,
		"http://[::1]:8080":     true,
		"http://api.wedos.com":  false,
		"http://192.0.2.1":      false,
		"ftp://api.wedos.com":   false,
		"api.wedos.com":         false,
	} {
		_, err := New(Config{BaseURL: raw, AccessToken: "t"})
		if (err == nil) != ok {
			t.Errorf("%s: accepted=%v, want %v (err %v)", raw, err == nil, ok, err)
		}
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	defer redirector.Close()

	c, err := New(Config{BaseURL: redirector.URL, AccessToken: "secret", MaxRetries: -1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListRows(t.Context(), "example.cz"); err == nil {
		t.Fatal("want an error for a redirect")
	}
	if hits.Load() != 0 {
		t.Fatal("redirect was followed with the Authorization header")
	}
}
