package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/skip-pay/terraform-provider-wedos/internal/client"
)

// zoneStore serialises writes per zone and caches each zone's row list for
// the lifetime of the provider process (one plan or one apply).
//
// The API has no way to fetch the rows of one name, so without the cache every
// record set would list the whole zone on every read, and a plan over a zone
// with N record sets would spend N requests of the 1,000/hour budget instead
// of one. Writes go through the store and update the cached list in place.
//
// The API has no concurrency guard either; the per-zone lock keeps parallel
// resources of one apply from interleaving their list-compare-write steps.
type zoneStore struct {
	client *client.Client
	policy commitPolicy

	mu    sync.Mutex
	zones map[string]*zoneEntry
}

// commitPolicy decides what happens after a zone's rows were written.
type commitPolicy struct {
	// commit publishes the zone after each write; without it WEDOS autocommits
	// about five minutes after the last change.
	commit bool
	// waitReload polls the zone after the commit until the API reports the
	// commit processed. Unlike Route 53 GetChange → INSYNC, this says nothing
	// about the name servers, which the API does not expose.
	waitReload    bool
	reloadTimeout time.Duration
	pollInterval  time.Duration
}

// ReloadTimeoutError means the commit succeeded but the reload was not
// observed in time. The change is not lost — WEDOS reloads it later — so
// callers report it as a warning, not as a failed write.
type ReloadTimeoutError struct {
	Zone    string
	Timeout time.Duration
}

func (e *ReloadTimeoutError) Error() string {
	return fmt.Sprintf("zone %s was committed, but the WEDOS API did not report it processed within %s; WEDOS will finish on its own", e.Zone, e.Timeout)
}

func IsReloadTimeout(err error) bool {
	var t *ReloadTimeoutError
	return errors.As(err, &t)
}

type zoneEntry struct {
	mu     sync.Mutex
	loaded bool
	rows   []client.Row
}

func newZoneStore(c *client.Client, p commitPolicy) *zoneStore {
	return &zoneStore{client: c, policy: p, zones: map[string]*zoneEntry{}}
}

func (s *zoneStore) entry(zone string) *zoneEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(zone)
	e, ok := s.zones[key]
	if !ok {
		e = &zoneEntry{}
		s.zones[key] = e
	}
	return e
}

// zoneTx is a locked view of one zone's rows.
type zoneTx struct {
	store *zoneStore
	zone  string
	e     *zoneEntry
	dirty bool
}

// Lock takes the zone's lock and returns a transaction. Callers must Unlock.
func (s *zoneStore) Lock(zone string) *zoneTx {
	e := s.entry(zone)
	e.mu.Lock()
	return &zoneTx{store: s, zone: zone, e: e}
}

func (tx *zoneTx) Unlock() { tx.e.mu.Unlock() }

// Forget drops the cached rows, e.g. after the zone was deleted.
func (tx *zoneTx) Forget() {
	tx.e.loaded = false
	tx.e.rows = nil
}

func (tx *zoneTx) rows(ctx context.Context) ([]client.Row, error) {
	if !tx.e.loaded {
		rows, err := tx.store.client.ListRows(ctx, tx.zone)
		if err != nil {
			return nil, err
		}
		tx.e.rows = rows
		tx.e.loaded = true
	}
	return tx.e.rows, nil
}

// All returns a copy of every row in the zone.
func (tx *zoneTx) All(ctx context.Context) ([]client.Row, error) {
	all, err := tx.rows(ctx)
	if err != nil {
		return nil, err
	}
	return append([]client.Row(nil), all...), nil
}

// Find returns the rows with the given relative name and type.
func (tx *zoneTx) Find(ctx context.Context, name, typ string) ([]client.Row, error) {
	all, err := tx.rows(ctx)
	if err != nil {
		return nil, err
	}
	var out []client.Row
	for _, r := range all {
		if strings.EqualFold(r.Name, name) && strings.EqualFold(r.Type, typ) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (tx *zoneTx) Add(ctx context.Context, in client.RowCreate) error {
	id, err := tx.store.client.AddRow(ctx, tx.zone, in)
	if err != nil {
		return err
	}
	tx.dirty = true
	if id == 0 {
		// The documented response carries row_id; if it ever does not, reload.
		tx.Forget()
		return nil
	}
	tx.e.rows = append(tx.e.rows, client.Row{
		ID: client.FlexInt(id), Name: in.Name, TTL: client.FlexInt(in.TTL), Type: in.Type, Rdata: in.Rdata, AuthorComment: in.AuthorComment,
	})
	return nil
}

func (tx *zoneTx) Update(ctx context.Context, id int64, in client.RowUpdate) error {
	if err := tx.store.client.UpdateRow(ctx, tx.zone, id, in); err != nil {
		return err
	}
	tx.dirty = true
	for i := range tx.e.rows {
		if int64(tx.e.rows[i].ID) == id {
			tx.e.rows[i].TTL = client.FlexInt(in.TTL)
			tx.e.rows[i].Rdata = in.Rdata
			tx.e.rows[i].AuthorComment = in.AuthorComment
		}
	}
	return nil
}

func (tx *zoneTx) Delete(ctx context.Context, id int64) error {
	err := tx.store.client.DeleteRow(ctx, tx.zone, id)
	if err != nil && !client.HasCode(err, 3309) {
		return err
	}
	tx.dirty = true
	rows := tx.e.rows[:0]
	for _, r := range tx.e.rows {
		if int64(r.ID) != id {
			rows = append(rows, r)
		}
	}
	tx.e.rows = rows
	return nil
}

// MarkDirty forces the next Commit, e.g. for a newly created zone, which WEDOS
// stages like a row change.
func (tx *zoneTx) MarkDirty() { tx.dirty = true }

// Commit publishes the zone when this transaction changed anything and the
// provider is configured to commit, then optionally waits until the API
// reports the commit processed.
//
// Measured on the live API: right after a commit the zone shows
// reload_pending=1 with the old serial; 7–10 s later reload_pending=0 and the
// serial is incremented. "Processed" is therefore reload_pending=0 with a
// serial different from the one before the commit; the extra GET before the
// commit makes this race-free. It is API-side state only: the name servers
// may serve the change minutes later.
func (tx *zoneTx) Commit(ctx context.Context) error {
	p := tx.store.policy
	if !tx.dirty || !p.commit {
		return nil
	}
	var before *client.Domain
	if p.waitReload {
		d, err := tx.store.client.GetDomain(ctx, tx.zone)
		if err != nil {
			return err
		}
		before = d
	}
	if err := tx.store.client.Commit(ctx, tx.zone); err != nil {
		return err
	}
	tx.dirty = false
	if before == nil {
		return nil
	}

	// Nothing was staged any more when we committed (WEDOS autocommitted in
	// between): only an already running reload is worth waiting for.
	settled := func(d *client.Domain) bool {
		if d.ReloadPending != 0 {
			return false
		}
		return d.Serial != before.Serial || (before.Changed == 0 && before.ReloadPending == 0)
	}
	deadline := time.Now().Add(p.reloadTimeout)
	for {
		if err := sleepCtx(ctx, p.pollInterval); err != nil {
			return err
		}
		d, err := tx.store.client.GetDomain(ctx, tx.zone)
		if err != nil {
			return err
		}
		if settled(d) {
			return nil
		}
		if time.Now().After(deadline) {
			return &ReloadTimeoutError{Zone: tx.zone, Timeout: p.reloadTimeout}
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
