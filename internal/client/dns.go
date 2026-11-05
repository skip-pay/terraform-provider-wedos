package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

const dnsPrefix = "/v2/dns/domains"

// Supported record types per the REST API – DNS documentation.
var RecordTypes = []string{"A", "AAAA", "ALIAS", "CAA", "CNAME", "DNAME", "HTTPS", "MX", "NAPTR", "NS", "SRV", "SSHFP", "TLSA", "TXT"}

type DomainSummary struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Type   string `json:"type"`
}

// Domain is the zone detail. The live API differs from the documentation:
// numbers come as strings, serial is "" until the first commit, tlock is a
// word ("none"), and axfr_ips is an array. Undocumented fields are not mapped.
type Domain struct {
	Name          string      `json:"name"`
	Status        string      `json:"status"`
	Type          string      `json:"type"`
	Autocommit    FlexInt     `json:"autocommit"`
	AXFREnabled   FlexInt     `json:"axfr_enabled"`
	AXFRIPs       FlexStrings `json:"axfr_ips"`
	Changed       FlexInt     `json:"changed"`
	CommitDate    *string     `json:"commit_date"`
	ReloadPending FlexInt     `json:"reload_pending"`
	ReloadDate    *string     `json:"reload_date"`
	Serial        FlexInt     `json:"serial"`
	RRCount       FlexInt     `json:"rr_cnt"`
	NS            []string    `json:"ns"`
}

type Row struct {
	ID            FlexInt `json:"ID"`
	Name          string  `json:"name"`
	TTL           FlexInt `json:"ttl"`
	Type          string  `json:"rdtype"`
	Rdata         string  `json:"rdata"`
	ChangedDate   *string `json:"changed_date"`
	AuthorComment string  `json:"author_comment"`
}

type DomainCreate struct {
	Name        string   `json:"name"`
	Type        string   `json:"type,omitempty"`
	AXFREnabled *int     `json:"axfr_enabled,omitempty"`
	AXFRIPs     []string `json:"axfr_ips,omitempty"`
}

type DomainUpdate struct {
	AXFREnabled *int     `json:"axfr_enabled,omitempty"`
	AXFRIPs     []string `json:"axfr_ips,omitempty"`
}

type RowCreate struct {
	Name          string `json:"name"`
	TTL           int64  `json:"ttl"`
	Type          string `json:"type"`
	Rdata         string `json:"rdata"`
	AuthorComment string `json:"author_comment,omitempty"`
}

// RowUpdate is a full replace of the mutable fields: the API requires both
// ttl and rdata. AuthorComment is always sent so it can be cleared.
type RowUpdate struct {
	TTL           int64  `json:"ttl"`
	Rdata         string `json:"rdata"`
	AuthorComment string `json:"author_comment"`
}

func domainPath(name string) string { return dnsPrefix + "/" + url.PathEscape(name) }

// ListDomains returns every DNS domain of the account, following pages.
func (c *Client) ListDomains(ctx context.Context) ([]DomainSummary, error) {
	const pageLimit = 100
	var all []DomainSummary
	for page := 1; ; page++ {
		var out struct {
			Domain []DomainSummary `json:"domain"`
			Total  FlexInt         `json:"total"`
		}
		q := url.Values{"page": {strconv.Itoa(page)}, "page_limit": {strconv.Itoa(pageLimit)}}
		if err := c.do(ctx, http.MethodGet, dnsPrefix, q, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Domain...)
		if len(out.Domain) == 0 || len(all) >= int(out.Total) {
			return all, nil
		}
	}
}

func (c *Client) GetDomain(ctx context.Context, name string) (*Domain, error) {
	var out struct {
		Domain Domain `json:"domain"`
	}
	if err := c.do(ctx, http.MethodGet, domainPath(name), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Domain, nil
}

func (c *Client) AddDomain(ctx context.Context, in DomainCreate) error {
	return c.do(ctx, http.MethodPost, dnsPrefix+"/add", nil, in, nil)
}

func (c *Client) UpdateDomain(ctx context.Context, name string, in DomainUpdate) error {
	return c.do(ctx, http.MethodPatch, domainPath(name), nil, in, nil)
}

func (c *Client) DeleteDomain(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, domainPath(name), nil, nil, nil)
}

// Commit publishes the staged changes of a domain.
func (c *Client) Commit(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, domainPath(name)+"/commit", nil, nil, nil)
}

func (c *Client) ListRows(ctx context.Context, domain string) ([]Row, error) {
	var out struct {
		Row []Row `json:"row"`
	}
	if err := c.do(ctx, http.MethodGet, domainPath(domain)+"/rows", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Row, nil
}

// AddRow creates a record and returns its row ID.
func (c *Client) AddRow(ctx context.Context, domain string, in RowCreate) (int64, error) {
	var out struct {
		RowID FlexInt `json:"row_id"`
	}
	if err := c.do(ctx, http.MethodPost, domainPath(domain)+"/rows", nil, in, &out); err != nil {
		return 0, err
	}
	return int64(out.RowID), nil
}

func (c *Client) UpdateRow(ctx context.Context, domain string, id int64, in RowUpdate) error {
	return c.do(ctx, http.MethodPatch, domainPath(domain)+"/rows/"+strconv.FormatInt(id, 10), nil, in, nil)
}

func (c *Client) DeleteRow(ctx context.Context, domain string, id int64) error {
	return c.do(ctx, http.MethodDelete, domainPath(domain)+"/rows/"+strconv.FormatInt(id, 10), nil, nil, nil)
}
