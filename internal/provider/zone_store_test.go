package provider

import (
	"fmt"
	"testing"
	"time"

	"github.com/skip-pay/terraform-provider-wedos/internal/client"
	"github.com/skip-pay/terraform-provider-wedos/internal/wedosmock"
)

// Reading N record sets of one zone must cost one row list, not N.
func TestZoneStoreListsZoneOnce(t *testing.T) {
	s := wedosmock.New()
	defer s.Close()
	s.AddZone("example.cz")
	for i := 0; i < 20; i++ {
		s.AddRow("example.cz", wedosmock.Row{Name: fmt.Sprintf("host%d", i), TTL: 300, Type: "A", Rdata: fmt.Sprintf("192.0.2.%d", i+1)})
	}
	c, err := client.New(client.Config{BaseURL: s.URL, ClientID: s.ClientID, ClientSecret: s.ClientSecret})
	if err != nil {
		t.Fatal(err)
	}
	store := newZoneStore(c, commitPolicy{commit: true})
	ctx := t.Context()

	for i := 0; i < 20; i++ {
		tx := store.Lock("example.cz")
		rows, err := tx.Find(ctx, fmt.Sprintf("host%d", i), "A")
		tx.Unlock()
		if err != nil || len(rows) != 1 {
			t.Fatalf("host%d: rows=%v err=%v", i, rows, err)
		}
	}
	if got := s.Calls("GET /domains/{domain}/rows"); got != 1 {
		t.Fatalf("want 1 row list, got %d", got)
	}

	// Writes keep the cache current without another list.
	tx := store.Lock("example.cz")
	if err := tx.Add(ctx, client.RowCreate{Name: "new", TTL: 300, Type: "A", Rdata: "198.51.100.1"}); err != nil {
		t.Fatal(err)
	}
	rows, _ := tx.Find(ctx, "new", "A")
	tx.Unlock()
	if len(rows) != 1 || rows[0].ID == 0 {
		t.Fatalf("cached row after add: %+v", rows)
	}
	if got := s.Calls("GET /domains/{domain}/rows"); got != 1 {
		t.Fatalf("write triggered a re-list: %d lists", got)
	}
}

func reloadStore(t *testing.T, s *wedosmock.Server, timeout time.Duration) *zoneStore {
	t.Helper()
	c, err := client.New(client.Config{BaseURL: s.URL, ClientID: s.ClientID, ClientSecret: s.ClientSecret})
	if err != nil {
		t.Fatal(err)
	}
	return newZoneStore(c, commitPolicy{commit: true, waitReload: true, reloadTimeout: timeout, pollInterval: time.Millisecond})
}

func TestCommitWaitsForReload(t *testing.T) {
	s := wedosmock.New()
	defer s.Close()
	s.AddZone("example.cz")
	s.ReloadPolls = 3
	store := reloadStore(t, s, time.Second)
	ctx := t.Context()

	tx := store.Lock("example.cz")
	defer tx.Unlock()
	if err := tx.Add(ctx, client.RowCreate{Name: "www", TTL: 300, Type: "A", Rdata: "192.0.2.1"}); err != nil {
		t.Fatal(err)
	}
	before := s.Calls("GET /domains/{domain}")
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// 1 read before the commit, then polls until the 4th read sees the reload.
	if got := s.Calls("GET /domains/{domain}") - before; got != 5 {
		t.Fatalf("want 5 zone reads, got %d", got)
	}
}

func TestCommitReloadTimeoutIsDistinct(t *testing.T) {
	s := wedosmock.New()
	defer s.Close()
	s.AddZone("example.cz")
	s.ReloadPolls = 1_000_000
	store := reloadStore(t, s, 20*time.Millisecond)
	ctx := t.Context()

	tx := store.Lock("example.cz")
	defer tx.Unlock()
	if err := tx.Add(ctx, client.RowCreate{Name: "www", TTL: 300, Type: "A", Rdata: "192.0.2.1"}); err != nil {
		t.Fatal(err)
	}
	err := tx.Commit(ctx)
	if !IsReloadTimeout(err) {
		t.Fatalf("want reload timeout, got %v", err)
	}
	if len(s.PublishedRows("example.cz")) != 1 {
		t.Fatal("the commit itself must have gone through")
	}
}

// If WEDOS autocommitted between our write and our commit, nothing is staged
// and the serial may never change again: the wait must not hang.
func TestCommitAfterAutocommitDoesNotHang(t *testing.T) {
	s := wedosmock.New()
	defer s.Close()
	s.AddZone("example.cz")
	store := reloadStore(t, s, time.Second)
	ctx := t.Context()

	tx := store.Lock("example.cz")
	defer tx.Unlock()
	if err := tx.Add(ctx, client.RowCreate{Name: "www", TTL: 300, Type: "A", Rdata: "192.0.2.1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.client.Commit(ctx, "example.cz"); err != nil { // the "autocommit"
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // let that reload finish
		if _, err := store.client.GetDomain(ctx, "example.cz"); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit after autocommit: %v", err)
	}
}
