package provider

import "testing"

func TestCanonMatchesLiveRewrites(t *testing.T) {
	// Pairs observed on the live API: sent value, stored value.
	cases := []struct{ typ, sent, stored string }{
		{"CNAME", "alpha.example.cz.", "alpha.example.cz"},
		{"CNAME", "Mixed.Case.Example.NET.", "mixed.case.example.net"},
		{"NS", "ns1.example.net.", "ns1.example.net"},
		{"MX", "10 mx1.example.net.", "10 mx1.example.net"},
		{"MX", "10 MX1.Example.NET.", "10 mx1.example.net"},
		{"SRV", "10 5 5060 sip.example.net.", "10 5 5060 sip.example.net"},
		{"TXT", `"already quoted"`, `"already quoted"`},
		{"AAAA", "2001:DB8:0:0::1", "2001:db8::1"},
		{"CAA", `0 issue "letsencrypt.org"`, `0 issue "letsencrypt.org"`},
	}
	for _, c := range cases {
		if canon(c.typ, c.sent) != canon(c.typ, c.stored) {
			t.Errorf("%s %q vs stored %q: canon differs", c.typ, c.sent, c.stored)
		}
	}
	if wire("ALIAS", "d1.cloudfront.net.") != "d1.cloudfront.net" || wire("CNAME", "a.example.net.") != "a.example.net." {
		t.Error("only ALIAS values are rewritten before sending")
	}
	if canon("TXT", "Hello.") == canon("TXT", "hello") {
		t.Error("TXT must not be normalised")
	}
}

func TestTypesConflict(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"CNAME", "A", true}, {"A", "CNAME", true}, {"CNAME", "TXT", true}, {"CNAME", "CNAME", false},
		{"ALIAS", "A", true}, {"AAAA", "ALIAS", true}, {"ALIAS", "TXT", false}, {"ALIAS", "MX", false},
		{"A", "TXT", false}, {"A", "AAAA", false},
	} {
		if got := typesConflict(c.a, c.b); got != c.want {
			t.Errorf("typesConflict(%s, %s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
