package provider

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/skip-pay/terraform-provider-wedos/internal/client"
)

// WEDOS rewrites host names inside rdata before storing them (live API,
// 2026-10-20): the trailing dot is dropped for CNAME, NS, MX and SRV targets,
// and CNAME targets are lowercased ("Mixed.Case.Example.NET." is stored as
// "mixed.case.example.net"). canon() applies the same rewrite to both sides of
// every comparison, and observe() keeps the configured spelling in state, so
// none of it shows up as a diff.
//
// hostTypes have a single host name as rdata; targetTypes end with one.
var (
	hostTypes   = map[string]bool{"CNAME": true, "ALIAS": true, "DNAME": true, "NS": true}
	targetTypes = map[string]bool{"MX": true, "SRV": true}
)

func canonHost(h string) string { return strings.ToLower(strings.TrimSuffix(h, ".")) }

// wire returns the value to send. ALIAS is the one type where WEDOS rejects a
// trailing dot (2314) instead of dropping it, so the provider drops it.
func wire(typ, v string) string {
	if strings.EqualFold(typ, "ALIAS") {
		return strings.TrimSuffix(v, ".")
	}
	return v
}

// canon returns the form of v that WEDOS stores, for comparison only.
func canon(typ, v string) string {
	t := strings.ToUpper(typ)
	switch {
	case t == "A" || t == "AAAA":
		if addr, err := netip.ParseAddr(v); err == nil {
			return addr.String()
		}
		return v
	case hostTypes[t]:
		return canonHost(v)
	case targetTypes[t]:
		fields := strings.Fields(v)
		if len(fields) == 0 {
			return v
		}
		fields[len(fields)-1] = canonHost(fields[len(fields)-1])
		return strings.Join(fields, " ")
	}
	return v
}

// desiredSet is the configured state of one (name, type) in a zone.
type desiredSet struct {
	name, typ string
	ttl       int64
	values    []string
	comment   string
}

type rowWrite struct {
	row   client.Row // zero for an add
	set   desiredSet
	value string
}

// syncPlan is the list of row writes that turn the existing rows of one or
// more record sets into the desired ones. Both resources build one and run it
// under the zone lock.
//
// Rows whose value is no longer wanted are rewritten in place to a new value
// (PATCH keeps the row), which is one request instead of two and is the only
// way to change a CNAME or ALIAS: adding a second CNAME next to the old one is
// rejected with 2313. Only surplus values are added or deleted.
//
// Execution order matters, because WEDOS can autocommit any intermediate
// state: rewrites and additions run before deletions, so a name never answers
// with nothing. The exception is a delete that would block an addition — a
// CNAME cannot coexist with any other type at the same name, an ALIAS not
// with A or AAAA — which has to run first.
type syncPlan struct {
	zone     string
	rewrites []rowWrite
	adds     []rowWrite
	updates  []rowWrite
	deletes  []client.Row
}

// addSet plans the writes for one desired set given its existing rows.
func (p *syncPlan) addSet(existing []client.Row, d desiredSet) {
	// Keyed by canonical value; want maps it back to the configured spelling.
	want := map[string]string{}
	for _, v := range d.values {
		want[canon(d.typ, v)] = v
	}
	have := map[string]client.Row{}
	var surplus []client.Row
	for _, row := range existing {
		k := canon(d.typ, row.Rdata)
		if _, seen := have[k]; seen {
			surplus = append(surplus, row)
			continue
		}
		if _, wanted := want[k]; !wanted {
			surplus = append(surplus, row)
			continue
		}
		have[k] = row
	}
	var missing []string
	for _, v := range d.values {
		if _, ok := have[canon(d.typ, v)]; !ok {
			missing = append(missing, v)
		}
	}
	sort.Strings(missing)
	sort.Slice(surplus, func(i, j int) bool { return surplus[i].ID < surplus[j].ID })

	for len(missing) > 0 && len(surplus) > 0 {
		p.rewrites = append(p.rewrites, rowWrite{row: surplus[0], set: d, value: missing[0]})
		surplus, missing = surplus[1:], missing[1:]
	}
	for _, v := range missing {
		p.adds = append(p.adds, rowWrite{set: d, value: v})
	}
	keys := make([]string, 0, len(have))
	for k := range have {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		row := have[k]
		if int64(row.TTL) != d.ttl || row.AuthorComment != d.comment {
			p.updates = append(p.updates, rowWrite{row: row, set: d, value: want[k]})
		}
	}
	p.deletes = append(p.deletes, surplus...)
}

// removeRows plans the deletion of rows no longer wanted at all.
func (p *syncPlan) removeRows(rows []client.Row) {
	p.deletes = append(p.deletes, rows...)
}

func (p *syncPlan) empty() bool {
	return len(p.rewrites)+len(p.adds)+len(p.updates)+len(p.deletes) == 0
}

// typesConflict reports whether rows of types a and b cannot exist at the same
// name: CNAME excludes every other type (2313), ALIAS excludes A and AAAA
// (2329, live API).
func typesConflict(a, b string) bool {
	a, b = strings.ToUpper(a), strings.ToUpper(b)
	if a == b {
		return false
	}
	if a == "CNAME" || b == "CNAME" {
		return true
	}
	addr := func(t string) bool { return t == "A" || t == "AAAA" }
	return (a == "ALIAS" && addr(b)) || (b == "ALIAS" && addr(a))
}

func (p *syncPlan) label(name, typ string) string {
	return fqdn(p.zone, name) + " " + typ
}

func (p *syncPlan) execute(ctx context.Context, tx *zoneTx) error {
	var early, late []client.Row
	for _, row := range p.deletes {
		blocking := false
		for _, a := range p.adds {
			if strings.EqualFold(a.set.name, row.Name) && typesConflict(a.set.typ, row.Type) {
				blocking = true
				break
			}
		}
		if blocking {
			early = append(early, row)
		} else {
			late = append(late, row)
		}
	}

	del := func(row client.Row) error {
		if err := tx.Delete(ctx, int64(row.ID)); err != nil {
			return fmt.Errorf("deleting %s %q: %w", p.label(row.Name, row.Type), row.Rdata, err)
		}
		return nil
	}
	for _, row := range early {
		if err := del(row); err != nil {
			return err
		}
	}
	for _, w := range p.rewrites {
		if err := tx.Update(ctx, int64(w.row.ID), client.RowUpdate{TTL: w.set.ttl, Rdata: wire(w.set.typ, w.value), AuthorComment: w.set.comment}); err != nil {
			return fmt.Errorf("changing %s %q to %q: %w", p.label(w.set.name, w.set.typ), w.row.Rdata, w.value, err)
		}
	}
	for _, w := range p.adds {
		if err := tx.Add(ctx, client.RowCreate{Name: w.set.name, TTL: w.set.ttl, Type: w.set.typ, Rdata: wire(w.set.typ, w.value), AuthorComment: w.set.comment}); err != nil {
			return fmt.Errorf("adding %s %q: %w", p.label(w.set.name, w.set.typ), w.value, err)
		}
	}
	for _, w := range p.updates {
		if err := tx.Update(ctx, int64(w.row.ID), client.RowUpdate{TTL: w.set.ttl, Rdata: wire(w.set.typ, w.value), AuthorComment: w.set.comment}); err != nil {
			return fmt.Errorf("updating %s %q: %w", p.label(w.set.name, w.set.typ), w.value, err)
		}
	}
	for _, row := range late {
		if err := del(row); err != nil {
			return err
		}
	}
	return nil
}

// observed is the Terraform view of the rows of one (name, type). Rows that
// disagree on TTL or comment are reported with values no configuration can
// have — TTL 0 and comment "<mixed>" — so the next plan shows a diff and the
// update makes the rows uniform again.
type observed struct {
	values  types.Set
	ttl     types.Int64
	comment types.String
}

// preferred are the values as written in the configuration or prior state: a
// stored value equal to one of them after canon() is reported in that
// spelling, so the trailing-dot normalisation does not show up as a diff.
func observe(rows []client.Row, preferred []string) observed {
	spelling := map[string]string{}
	for _, p := range preferred {
		spelling[canon(rows[0].Type, p)] = p
	}
	values := make([]attr.Value, 0, len(rows))
	ttl, comment := int64(rows[0].TTL), rows[0].AuthorComment
	uniformTTL, uniformComment := true, true
	for _, row := range rows {
		v := row.Rdata
		if p, ok := spelling[canon(row.Type, v)]; ok {
			v = p
		}
		values = append(values, types.StringValue(v))
		uniformTTL = uniformTTL && int64(row.TTL) == ttl
		uniformComment = uniformComment && row.AuthorComment == comment
	}
	o := observed{values: types.SetValueMust(types.StringType, values)}
	if uniformTTL {
		o.ttl = types.Int64Value(ttl)
	} else {
		o.ttl = types.Int64Value(0)
	}
	switch {
	case !uniformComment:
		o.comment = types.StringValue("<mixed>")
	case comment == "":
		o.comment = types.StringNull()
	default:
		o.comment = types.StringValue(comment)
	}
	return o
}
