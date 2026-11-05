// Package wedosmock is an in-memory fake of the WEDOS REST API v2 DNS
// endpoints, built from the public documentation (kb.wedos.global/api-dns/,
// kb.wedos.global/api-manual/) and from live observations of the envelope:
// HTTP 200 for everything, `code` as a string.
//
// Behaviour that the documentation leaves open is modelled conservatively:
// row writes are staged and only Commit publishes them; there is no
// autocommit timer.
package wedosmock

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var supportedTypes = []string{"A", "AAAA", "ALIAS", "CAA", "CNAME", "DNAME", "HTTPS", "MX", "NAPTR", "NS", "SRV", "SSHFP", "TLSA", "TXT"}

var commentRe = regexp.MustCompile(`^[A-Za-z0-9 ]*$`)

// Behaviour measured on the live API (2026-10-20).
const (
	minTTL = 300
	maxTTL = 172800
)

var (
	hostTypes   = map[string]bool{"CNAME": true, "ALIAS": true, "DNAME": true, "NS": true}
	targetTypes = map[string]bool{"MX": true, "SRV": true}
	caaRe       = regexp.MustCompile(`^\d{1,3} [A-Za-z0-9]+ ".*"$`)
)

// storedRdata mirrors WEDOS rewriting host names: trailing dot dropped and
// lowercased for CNAME, NS, MX and SRV targets (ALIAS with a dot is rejected
// before this point).
func storedRdata(typ, v string) string {
	switch {
	case hostTypes[typ]:
		return strings.ToLower(strings.TrimSuffix(v, "."))
	case targetTypes[typ]:
		fields := strings.Fields(v)
		if len(fields) > 0 {
			fields[len(fields)-1] = strings.ToLower(strings.TrimSuffix(fields[len(fields)-1], "."))
		}
		return strings.Join(fields, " ")
	}
	return v
}

func ttlOK(ttl int64) bool { return ttl >= minTTL && ttl <= maxTTL }

type Row struct {
	ID            int64
	Name          string
	TTL           int64
	Type          string
	Rdata         string
	AuthorComment string
}

type zone struct {
	name          string
	reloadPending int // GETs of the zone detail left until the reload finishes
	reloadDate    string
	axfrEnabled   int
	axfrIPs       string
	rows          map[int64]Row
	published     map[int64]Row
	changed       bool
	serial        int64
}

// Server is the fake API. Create it with New and point the provider at URL.
type Server struct {
	*httptest.Server

	ClientID     string
	ClientSecret string
	// ReloadPolls is how many zone-detail reads report reload_pending=1 after
	// a commit before the reload finishes and the serial is incremented —
	// the live API takes 7–10 s. 0 reloads at commit time.
	ReloadPolls int
	// FailRdata makes adding a row with exactly this value fail with 2314,
	// to test partially applied changes.
	FailRdata string

	mu        sync.Mutex
	zones     map[string]*zone
	nextRowID int64
	tokens    map[string]bool
	tokenSeq  int
	calls     map[string]int
}

func New() *Server {
	s := &Server{
		ClientID:     "test-client",
		ClientSecret: "test-secret",
		zones:        map[string]*zone{},
		nextRowID:    100,
		tokens:       map[string]bool{},
		calls:        map[string]int{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// --- helpers for tests -------------------------------------------------------

// AddZone creates a zone directly, bypassing the API.
func (s *Server) AddZone(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	z := newZone(name)
	z.changed = false
	z.serial = 2026010101
	s.zones[name] = z
}

// AddRow inserts a row and publishes it, bypassing the API. Returns the row ID.
func (s *Server) AddRow(zoneName string, r Row) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	z := s.zones[zoneName]
	s.nextRowID++
	r.ID = s.nextRowID
	z.rows[r.ID] = r
	z.published[r.ID] = r
	return r.ID
}

// DeleteRows removes rows by name and type, bypassing the API (simulates drift).
func (s *Server) DeleteRows(zoneName, name, typ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	z := s.zones[zoneName]
	for id, r := range z.rows {
		if r.Name == name && r.Type == typ {
			delete(z.rows, id)
			delete(z.published, id)
		}
	}
}

// PublishedRows returns the committed rows of a zone, sorted by name, type, rdata.
func (s *Server) PublishedRows(zoneName string) []Row {
	s.mu.Lock()
	defer s.mu.Unlock()
	z, ok := s.zones[zoneName]
	if !ok {
		return nil
	}
	return sortedRows(z.published)
}

// HasZone reports whether the zone exists.
func (s *Server) HasZone(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.zones[name]
	return ok
}

// Calls returns how many times "METHOD /path-pattern" was called.
func (s *Server) Calls(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[key]
}

// --- HTTP --------------------------------------------------------------------

type reply struct {
	code int
	msg  string
	data any
}

func ok(data any) reply               { return reply{code: 1000, msg: "OK", data: data} }
func fail(code int, msg string) reply { return reply{code: code, msg: msg} }

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var rep reply
	if r.URL.Path == "/oauth2/token" {
		rep = s.token(r)
	} else if !s.authorized(r) {
		rep = fail(2000, "Missing Authorization header (OAuth2 required for this endpoint)")
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			rep = reply{code: 3002, msg: "Invalid token", data: "invalid_token (The access token provided is invalid)"}
		}
	} else {
		rep = s.route(r)
	}

	s.tokenSeq++
	body := map[string]any{
		"code":       strconv.Itoa(rep.code),
		"msg":        rep.msg,
		"request_id": fmt.Sprintf("mock.%d", s.tokenSeq),
	}
	if rep.data != nil {
		body["data"] = rep.data
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) token(r *http.Request) reply {
	if r.Method != http.MethodPost {
		return reply{code: 2000, msg: "API method error - Bad Request", data: map[string]any{"status_code": 400, "error": "invalid_request"}}
	}
	_ = r.ParseForm()
	id, secret, basic := r.BasicAuth()
	if !basic {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if r.PostForm.Get("grant_type") != "client_credentials" {
		return reply{code: 2000, msg: "API method error - Bad Request", data: map[string]any{"status_code": 400, "error": "unsupported_grant_type"}}
	}
	if subtle.ConstantTimeCompare([]byte(id), []byte(s.ClientID)) != 1 || subtle.ConstantTimeCompare([]byte(secret), []byte(s.ClientSecret)) != 1 {
		return reply{code: 2000, msg: "API method error - Bad Request", data: map[string]any{"status_code": 400, "error": "invalid_client", "error_description": "The client credentials are invalid"}}
	}
	scope := r.PostForm.Get("scope")
	if scope == "" {
		scope = "wdns"
	}
	if scope != "wdns" {
		return reply{code: 2000, msg: "API method error - Bad Request", data: map[string]any{"status_code": 400, "error": "invalid_scope"}}
	}
	s.tokenSeq++
	tok := fmt.Sprintf("mock-token-%d", s.tokenSeq)
	s.tokens[tok] = true
	return ok(map[string]any{"access_token": tok, "expires_in": 3600, "token_type": "Bearer", "scope": scope})
}

func (s *Server) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	tok, found := strings.CutPrefix(h, "Bearer ")
	return found && s.tokens[tok]
}

// IssueToken returns a valid static token, as if generated in the admin.
func (s *Server) IssueToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenSeq++
	tok := fmt.Sprintf("static-token-%d", s.tokenSeq)
	s.tokens[tok] = true
	return tok
}

// RevokeTokens invalidates every issued token.
func (s *Server) RevokeTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = map[string]bool{}
}

func (s *Server) route(r *http.Request) reply {
	rest, found := strings.CutPrefix(r.URL.Path, "/v2/dns/domains")
	if !found {
		return fail(4000, "Unknown API method")
	}
	parts := []string{}
	for _, p := range strings.Split(strings.Trim(rest, "/"), "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}

	key := r.Method + " /domains"
	switch {
	case len(parts) == 1 && parts[0] == "add":
		key += "/add"
	case len(parts) >= 1:
		key += "/{domain}"
		if len(parts) >= 2 {
			key += "/" + parts[1]
		}
		if len(parts) == 3 {
			key += "/{row_id}"
		}
	}
	s.calls[key]++

	switch key {
	case "GET /domains":
		return s.listDomains(r)
	case "POST /domains/add":
		return s.addDomain(r)
	case "GET /domains/{domain}":
		return s.getDomain(parts[0])
	case "PATCH /domains/{domain}":
		return s.updateDomain(parts[0], r)
	case "DELETE /domains/{domain}":
		return s.deleteDomain(parts[0])
	case "POST /domains/{domain}/commit":
		return s.commit(parts[0])
	case "GET /domains/{domain}/rows":
		return s.listRows(parts[0])
	case "POST /domains/{domain}/rows":
		return s.addRow(parts[0], r)
	case "GET /domains/{domain}/rows/{row_id}":
		return s.getRow(parts[0], parts[2])
	case "PATCH /domains/{domain}/rows/{row_id}":
		return s.updateRow(parts[0], parts[2], r)
	case "DELETE /domains/{domain}/rows/{row_id}":
		return s.deleteRow(parts[0], parts[2])
	}
	return fail(4000, "Unknown API method")
}

func newZone(name string) *zone {
	return &zone{name: name, rows: map[int64]Row{}, published: map[int64]Row{}, changed: true}
}

func decode(r *http.Request, v any) error {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

func (s *Server) listDomains(r *http.Request) reply {
	names := make([]string, 0, len(s.zones))
	for n := range s.zones {
		names = append(names, n)
	}
	sort.Strings(names)
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("page_limit"))
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 50
	}
	out := []map[string]any{}
	for i := (page - 1) * limit; i < len(names) && i < page*limit; i++ {
		out = append(out, map[string]any{"name": names[i], "status": "active", "type": "primary"})
	}
	return ok(map[string]any{"domain": out, "total": strconv.Itoa(len(names)), "page": page, "page_limit": limit})
}

func (s *Server) addDomain(r *http.Request) reply {
	var in struct {
		Name        string `json:"name"`
		Type        string `json:"type"`
		AXFREnabled *int   `json:"axfr_enabled"`
		AXFRIPs     any    `json:"axfr_ips"`
	}
	if err := decode(r, &in); err != nil {
		return fail(4001, "Input validation failed")
	}
	if in.Name == "" {
		return fail(2100, "Required parameter missing")
	}
	if !strings.Contains(in.Name, ".") {
		return fail(2202, "Invalid domain name")
	}
	if in.Type != "" && in.Type != "primary" {
		return fail(2301, "Invalid format of type")
	}
	if _, exists := s.zones[in.Name]; exists {
		return fail(3303, "Domain already exists in WDNS")
	}
	z := newZone(in.Name)
	if in.AXFREnabled != nil && *in.AXFREnabled == 1 {
		ips := joinIPs(in.AXFRIPs)
		if ips == "" {
			return fail(2305, "Required parameter axfr_ips missing")
		}
		z.axfrEnabled, z.axfrIPs = 1, ips
	}
	s.zones[in.Name] = z
	return ok(map[string]any{"domain_id": 40000 + len(s.zones), "name": in.Name})
}

func joinIPs(v any) string {
	switch t := v.(type) {
	case string:
		return strings.Join(strings.Fields(t), " ")
	case []any:
		var out []string
		for _, x := range t {
			out = append(out, fmt.Sprint(x))
		}
		return strings.Join(out, " ")
	}
	return ""
}

// tickReload advances a pending reload by one step; the last step increments
// the serial, as the live API does at reload time, not at commit time.
func (z *zone) tickReload() {
	if z.reloadPending == 0 {
		return
	}
	z.reloadPending--
	if z.reloadPending == 0 {
		if z.serial == 0 {
			z.serial = 2026010100
		}
		z.serial++
		z.reloadDate = fmt.Sprintf("2026-10-20 12:%02d:00", z.serial%60)
	}
}

func (s *Server) getDomain(name string) reply {
	z, found := s.zones[name]
	if !found {
		return fail(3222, "Failed to open the domain")
	}
	pending := 0
	if z.reloadPending > 0 {
		pending = 1
	}
	defer z.tickReload()
	// Shape copied from the live API (2026-10-20), not from the documentation:
	// numbers as strings, tlock as a word, axfr_ips as an array, serial "" until
	// the first commit.
	serial := ""
	if z.serial > 0 {
		serial = strconv.FormatInt(z.serial, 10)
	}
	axfrIPs := strings.Fields(z.axfrIPs)
	if axfrIPs == nil {
		axfrIPs = []string{}
	}
	return ok(map[string]any{"domain": map[string]any{
		"name": z.name, "status": "active", "type": "primary", "tlock": "none", "autocommit": "1",
		"axfr_enabled": strconv.Itoa(z.axfrEnabled), "axfr_ips": axfrIPs, "changed": strconv.Itoa(boolInt(z.changed)),
		"serial": serial, "rr_cnt": strconv.Itoa(len(z.published)), "reload_pending": pending, "reload_date": nilIfEmpty(z.reloadDate), "ns_out": "4",
		"ns": []string{"ns.wedos.net", "ns.wedos.cz", "ns.wedos.eu", "ns.wedos.com"},
	}})
}

func (s *Server) updateDomain(name string, r *http.Request) reply {
	z, found := s.zones[name]
	if !found {
		return fail(3222, "Failed to open the domain")
	}
	var in struct {
		AXFREnabled *int `json:"axfr_enabled"`
		AXFRIPs     any  `json:"axfr_ips"`
	}
	if err := decode(r, &in); err != nil {
		return fail(4001, "Input validation failed")
	}
	if in.AXFREnabled != nil {
		if *in.AXFREnabled == 1 {
			ips := joinIPs(in.AXFRIPs)
			if ips == "" {
				return fail(2305, "Required parameter axfr_ips missing")
			}
			z.axfrIPs = ips
		} else {
			z.axfrIPs = ""
		}
		z.axfrEnabled = *in.AXFREnabled
	}
	return ok(nil)
}

func (s *Server) deleteDomain(name string) reply {
	if _, found := s.zones[name]; !found {
		return fail(3222, "Failed to open the domain")
	}
	delete(s.zones, name)
	return ok(nil)
}

func (s *Server) commit(name string) reply {
	z, found := s.zones[name]
	if !found {
		return fail(3222, "Failed to open the domain")
	}
	z.published = map[int64]Row{}
	for id, row := range z.rows {
		z.published[id] = row
	}
	if z.changed {
		z.reloadPending = s.ReloadPolls + 1
		z.tickReload()
	}
	z.changed = false
	return ok(nil)
}

func rowJSON(r Row) map[string]any {
	return map[string]any{"ID": strconv.FormatInt(r.ID, 10), "name": r.Name, "ttl": strconv.FormatInt(r.TTL, 10), "rdtype": r.Type, "rdata": r.Rdata, "changed_date": "2026-10-20 12:00:00", "author_comment": r.AuthorComment}
}

func sortedRows(m map[int64]Row) []Row {
	out := make([]Row, 0, len(m))
	for _, r := range m {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		return a.Rdata < b.Rdata
	})
	return out
}

func (s *Server) listRows(name string) reply {
	z, found := s.zones[name]
	if !found {
		return fail(3222, "Failed to open the domain")
	}
	out := []map[string]any{}
	for _, r := range sortedRows(z.rows) {
		out = append(out, rowJSON(r))
	}
	return ok(map[string]any{"row": out})
}

func (s *Server) lookupRow(zoneName, idStr string) (*zone, Row, *reply) {
	z, found := s.zones[zoneName]
	if !found {
		rep := fail(3222, "Failed to open the domain")
		return nil, Row{}, &rep
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		rep := fail(2101, "Invalid input parameter")
		return nil, Row{}, &rep
	}
	row, found := z.rows[id]
	if !found {
		rep := fail(3309, "Record with this ID does not exist")
		return nil, Row{}, &rep
	}
	return z, row, nil
}

func (s *Server) getRow(zoneName, idStr string) reply {
	_, row, rep := s.lookupRow(zoneName, idStr)
	if rep != nil {
		return *rep
	}
	return ok(map[string]any{"row": rowJSON(row)})
}

type rowInput struct {
	Name          *string `json:"name"`
	TTL           *int64  `json:"ttl"`
	Type          string  `json:"type"`
	Rdata         *string `json:"rdata"`
	AuthorComment *string `json:"author_comment"`
	AuthComment   *string `json:"auth_comment"`
}

func (in rowInput) comment() *string {
	if in.AuthorComment != nil {
		return in.AuthorComment
	}
	return in.AuthComment
}

func (s *Server) addRow(zoneName string, r *http.Request) reply {
	z, found := s.zones[zoneName]
	if !found {
		return fail(3222, "Failed to open the domain")
	}
	var in rowInput
	if err := decode(r, &in); err != nil {
		return fail(4001, "Input validation failed")
	}
	if in.Name == nil || in.TTL == nil || in.Type == "" || in.Rdata == nil {
		return fail(2100, "Required parameter missing")
	}
	if !slices.Contains(supportedTypes, in.Type) {
		return fail(2309, "Invalid record type")
	}
	if !ttlOK(*in.TTL) {
		return fail(2317, "Invalid request - WDNS domain - invalid TTL limit")
	}
	if *in.Rdata == "" || *in.Rdata == s.FailRdata || (in.Type == "CAA" && !caaRe.MatchString(*in.Rdata)) {
		return fail(2314, "Invalid request - WDNS domain - invalid rdata format for this rdtype")
	}
	if in.Type == "ALIAS" && *in.Name != "" {
		return fail(2312, "Invalid request - WDNS domain - invalid name format for this rdtype")
	}
	if in.Type == "ALIAS" && strings.HasSuffix(*in.Rdata, ".") {
		return fail(2314, "Invalid request - WDNS domain - invalid rdata format for this rdtype")
	}
	lowered := strings.ToLower(*in.Name)
	in.Name = &lowered
	if strings.HasSuffix(*in.Name, ".") || strings.Contains(*in.Name, " ") {
		return fail(2311, "Invalid record name")
	}
	if c := in.comment(); c != nil && !commentRe.MatchString(*c) {
		return fail(2298, "Parameter author_comment contains invalid characters")
	}
	rdata := storedRdata(in.Type, *in.Rdata)
	for _, row := range z.rows {
		if row.Name != *in.Name {
			continue
		}
		if row.Type == in.Type && row.Rdata == rdata {
			return fail(2316, "This record already exists")
		}
		if row.Type == "CNAME" || in.Type == "CNAME" {
			return fail(2313, "Invalid CNAME for this name")
		}
		addr := func(t string) bool { return t == "A" || t == "AAAA" }
		if (row.Type == "ALIAS" && addr(in.Type)) || (in.Type == "ALIAS" && addr(row.Type)) {
			return fail(2329, "Invalid request - WDNS domain - invalid combination of ALIAS with A/AAAA records")
		}
	}
	s.nextRowID++
	row := Row{ID: s.nextRowID, Name: *in.Name, TTL: *in.TTL, Type: in.Type, Rdata: rdata}
	if c := in.comment(); c != nil {
		row.AuthorComment = *c
	}
	z.rows[row.ID] = row
	z.changed = true
	return ok(map[string]any{"domain": zoneName, "row_id": row.ID})
}

func (s *Server) updateRow(zoneName, idStr string, r *http.Request) reply {
	z, row, rep := s.lookupRow(zoneName, idStr)
	if rep != nil {
		return *rep
	}
	var in rowInput
	if err := decode(r, &in); err != nil {
		return fail(4001, "Input validation failed")
	}
	if in.Name != nil || in.Type != "" {
		return fail(4001, "Input validation failed")
	}
	if in.TTL == nil || in.Rdata == nil {
		return fail(2100, "Required parameter missing")
	}
	if !ttlOK(*in.TTL) {
		return fail(2317, "Invalid request - WDNS domain - invalid TTL limit")
	}
	if (row.Type == "CAA" && !caaRe.MatchString(*in.Rdata)) || (row.Type == "ALIAS" && strings.HasSuffix(*in.Rdata, ".")) {
		return fail(2314, "Invalid request - WDNS domain - invalid rdata format for this rdtype")
	}
	if c := in.comment(); c != nil && !commentRe.MatchString(*c) {
		return fail(2298, "Parameter author_comment contains invalid characters")
	}
	rdata := storedRdata(row.Type, *in.Rdata)
	for id, other := range z.rows {
		if id != row.ID && other.Name == row.Name && other.Type == row.Type && other.Rdata == rdata {
			return fail(2316, "This record already exists")
		}
	}
	row.TTL, row.Rdata = *in.TTL, rdata
	if c := in.comment(); c != nil {
		row.AuthorComment = *c
	}
	z.rows[row.ID] = row
	z.changed = true
	return ok(nil)
}

func (s *Server) deleteRow(zoneName, idStr string) reply {
	z, row, rep := s.lookupRow(zoneName, idStr)
	if rep != nil {
		return *rep
	}
	delete(z.rows, row.ID)
	z.changed = true
	return ok(nil)
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
