package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

// TTL bounds measured on the live API (2026-10-20): 60–299 and 172801+ are
// rejected with 2317.
const (
	minTTL = 300
	maxTTL = 172800
)

// caaRe: WEDOS requires the CAA value quoted (2314 otherwise).
var caaRe = regexp.MustCompile(`^\d{1,3} [A-Za-z0-9]+ ".*"$`)

// validateRecordSet checks rules the API enforces per type, so they fail at
// plan time instead of halfway through an apply. Returns a message or "".
func validateRecordSet(name, typ string, values []string) string {
	seen := map[string]string{}
	for _, v := range values {
		if prev, dup := seen[canon(typ, v)]; dup {
			return fmt.Sprintf("Values %q and %q are the same record once WEDOS normalises them (trailing dot, case, address notation); keep one.", prev, v)
		}
		seen[canon(typ, v)] = v
	}
	switch strings.ToUpper(typ) {
	case "ALIAS":
		if name != "" {
			return "ALIAS is only accepted at the zone apex (name = \"\"); WEDOS rejects it elsewhere with 2312."
		}
	case "CAA":
		for _, v := range values {
			if !caaRe.MatchString(v) {
				return fmt.Sprintf(`CAA value %q must be <flags> <tag> "<value>" with the value quoted, e.g. 0 issue "letsencrypt.org".`, v)
			}
		}
	case "CNAME":
		if len(values) > 1 {
			return "A CNAME record set can hold only one value."
		}
	}
	return ""
}

var (
	domainNameRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)
	// Relative owner name: empty (apex), "*" or "*.label", or dot-separated
	// labels; underscores are allowed for _acme-challenge, _domainkey, SRV.
	recordNameRe = regexp.MustCompile(`^$|^(\*|[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?)(\.[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?)*$`)
	// author_comment accepts only ASCII letters, digits and spaces (code 2298).
	authorCommentRe = regexp.MustCompile(`^[A-Za-z0-9 ]*$`)
)

type domainNameValidator struct{}

func (domainNameValidator) Description(context.Context) string {
	return "must be a lowercase domain name without a trailing dot"
}
func (v domainNameValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}
func (v domainNameValidator) ValidateString(ctx context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if !domainNameRe.MatchString(req.ConfigValue.ValueString()) {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid zone name", v.Description(ctx))
	}
}

type recordNameValidator struct{}

func (recordNameValidator) Description(context.Context) string {
	return `must be a lowercase name relative to the zone: "" for the apex, "www", "_acme-challenge.api", "*" — no trailing dot and no zone suffix`
}
func (v recordNameValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}
func (v recordNameValidator) ValidateString(ctx context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if !recordNameRe.MatchString(req.ConfigValue.ValueString()) {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid record name", v.Description(ctx))
	}
}

type authorCommentValidator struct{}

func (authorCommentValidator) Description(context.Context) string {
	return "WEDOS accepts only ASCII letters, digits and spaces in author_comment"
}
func (v authorCommentValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}
func (v authorCommentValidator) ValidateString(ctx context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if !authorCommentRe.MatchString(req.ConfigValue.ValueString()) {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid author_comment", v.Description(ctx))
	}
}
