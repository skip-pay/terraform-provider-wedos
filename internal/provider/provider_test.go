package provider

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/skip-pay/terraform-provider-wedos/internal/wedosmock"
)

// Acceptance tests run real Terraform (TF_ACC=1, terraform in PATH) against
// the in-memory WEDOS fake, so they need no account and spend no budget.

var factories = map[string]func() (tfprotov6.ProviderServer, error){
	"wedos": providerserver.NewProtocol6WithError(New("test")()),
}

func providerBlock(s *wedosmock.Server) string {
	return fmt.Sprintf(`
provider "wedos" {
  base_url      = %q
  client_id     = %q
  client_secret = %q
}
`, s.URL, s.ClientID, s.ClientSecret)
}

func publishedValues(s *wedosmock.Server, name, typ string) []string {
	var out []string
	for _, r := range s.PublishedRows(testZone) {
		if r.Name == name && r.Type == typ {
			out = append(out, fmt.Sprintf("%d %s", r.TTL, r.Rdata))
		}
	}
	return out
}

// testZone is the zone every acceptance test works in.
const testZone = "example.cz"

func checkPublished(s *wedosmock.Server, name, typ string, want ...string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		got := publishedValues(s, name, typ)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			return fmt.Errorf("published %s %s in %s: got %v, want %v", name, typ, testZone, got, want)
		}
		return nil
	}
}

func TestAccZone(t *testing.T) {
	s := wedosmock.New()
	defer s.Close()

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy: func(*terraform.State) error {
			if s.HasZone("example.cz") {
				return fmt.Errorf("zone still exists")
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: providerBlock(s) + `
resource "wedos_dns_zone" "z" {
  name = "example.cz"
}
data "wedos_dns_zone" "z" {
  name = wedos_dns_zone.z.name
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("wedos_dns_zone.z", "id", "example.cz"),
					resource.TestCheckResourceAttr("wedos_dns_zone.z", "status", "active"),
					resource.TestCheckResourceAttr("wedos_dns_zone.z", "name_servers.#", "4"),
					resource.TestCheckNoResourceAttr("wedos_dns_zone.z", "axfr_ips"),
					resource.TestCheckResourceAttr("data.wedos_dns_zone.z", "type", "primary"),
				),
			},
			{
				Config: providerBlock(s) + `
resource "wedos_dns_zone" "z" {
  name     = "example.cz"
  axfr_ips = ["192.0.2.53", "198.51.100.53"]
}`,
				Check: resource.TestCheckResourceAttr("wedos_dns_zone.z", "axfr_ips.#", "2"),
			},
			{
				ResourceName:      "wedos_dns_zone.z",
				ImportState:       true,
				ImportStateId:     "example.cz",
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccZoneAlreadyExists(t *testing.T) {
	s := wedosmock.New()
	defer s.Close()
	s.AddZone("example.cz")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config:      providerBlock(s) + `resource "wedos_dns_zone" "z" { name = "example.cz" }`,
			ExpectError: regexp.MustCompile(`already exists in WEDOS DNS`),
		}},
	})
}

func TestAccRecordSet(t *testing.T) {
	s := wedosmock.New()
	defer s.Close()

	zone := `
resource "wedos_dns_zone" "z" {
  name = "example.cz"
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: providerBlock(s) + zone + `
resource "wedos_dns_record_set" "www" {
  zone    = wedos_dns_zone.z.name
  name    = "www"
  type    = "A"
  ttl     = 300
  records = ["192.0.2.1", "192.0.2.2"]
}
resource "wedos_dns_record_set" "apex_txt" {
  zone           = wedos_dns_zone.z.name
  name           = ""
  type           = "TXT"
  ttl            = 3600
  records        = ["v=spf1 -all", "google-site-verification=abc"]
  author_comment = "managed by terraform"
}
resource "wedos_dns_record_set" "apex_alias" {
  zone    = wedos_dns_zone.z.name
  name    = ""
  type    = "ALIAS"
  ttl     = 300
  records = ["d111111abcdef8.cloudfront.net."]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("wedos_dns_record_set.www", "id", "example.cz/www/A"),
					resource.TestCheckResourceAttr("wedos_dns_record_set.www", "fqdn", "www.example.cz"),
					resource.TestCheckResourceAttr("wedos_dns_record_set.apex_txt", "id", "example.cz/@/TXT"),
					checkPublished(s, "www", "A", "300 192.0.2.1", "300 192.0.2.2"),
					checkPublished(s, "", "TXT", "3600 google-site-verification=abc", "3600 v=spf1 -all"),
					checkPublished(s, "", "ALIAS", "300 d111111abcdef8.cloudfront.net"),
				),
			},
			{
				// Replace one value, keep one, change TTL: one PATCH rewrites
				// the dropped row in place, one PATCH updates the TTL.
				Config: providerBlock(s) + zone + `
resource "wedos_dns_record_set" "www" {
  zone    = wedos_dns_zone.z.name
  name    = "www"
  type    = "A"
  ttl     = 600
  records = ["192.0.2.1", "192.0.2.3", "192.0.2.4"]
}
resource "wedos_dns_record_set" "apex_txt" {
  zone    = wedos_dns_zone.z.name
  name    = ""
  type    = "TXT"
  ttl     = 3600
  records = ["v=spf1 -all"]
}
resource "wedos_dns_record_set" "apex_alias" {
  zone    = wedos_dns_zone.z.name
  name    = ""
  type    = "ALIAS"
  ttl     = 300
  records = ["d222222abcdef8.cloudfront.net."]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					checkPublished(s, "www", "A", "600 192.0.2.1", "600 192.0.2.3", "600 192.0.2.4"),
					checkPublished(s, "", "TXT", "3600 v=spf1 -all"),
					checkPublished(s, "", "ALIAS", "300 d222222abcdef8.cloudfront.net"),
					resource.TestCheckNoResourceAttr("wedos_dns_record_set.apex_txt", "author_comment"),
				),
			},
			{
				ResourceName:            "wedos_dns_record_set.www",
				ImportState:             true,
				ImportStateId:           "example.cz/www/A",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"allow_overwrite"},
			},
			{
				ResourceName:      "wedos_dns_record_set.apex_txt",
				ImportState:       true,
				ImportStateId:     "example.cz/@/TXT",
				ImportStateVerify: true,
			},
			{
				// Rows deleted outside Terraform are recreated.
				PreConfig: func() { s.DeleteRows("example.cz", "www", "A") },
				Config: providerBlock(s) + zone + `
resource "wedos_dns_record_set" "www" {
  zone    = wedos_dns_zone.z.name
  name    = "www"
  type    = "A"
  ttl     = 600
  records = ["192.0.2.1", "192.0.2.3", "192.0.2.4"]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					checkPublished(s, "www", "A", "600 192.0.2.1", "600 192.0.2.3", "600 192.0.2.4"),
					checkPublished(s, "", "TXT"),
					checkPublished(s, "", "ALIAS"),
				),
			},
		},
	})
}

func TestAccRecordSetCNAMEChange(t *testing.T) {
	s := wedosmock.New()
	defer s.Close()
	s.AddZone("example.cz")

	cfg := func(target string) string {
		return providerBlock(s) + fmt.Sprintf(`
resource "wedos_dns_record_set" "app" {
  zone    = "example.cz"
  name    = "app"
  type    = "CNAME"
  ttl     = 300
  records = [%q]
}`, target)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{Config: cfg("lb-1.example.net."), Check: checkPublished(s, "app", "CNAME", "300 lb-1.example.net")},
			// Adding a second CNAME before deleting the first would be rejected
			// with 2313; the provider rewrites the row in place instead.
			{Config: cfg("lb-2.example.net."), Check: checkPublished(s, "app", "CNAME", "300 lb-2.example.net")},
		},
	})
}

func TestAccRecordSetExistingRows(t *testing.T) {
	s := wedosmock.New()
	defer s.Close()
	s.AddZone("example.cz")
	s.AddRow("example.cz", wedosmock.Row{Name: "mail", TTL: 3600, Type: "A", Rdata: "192.0.2.25"})

	cfg := func(overwrite bool) string {
		return providerBlock(s) + fmt.Sprintf(`
resource "wedos_dns_record_set" "mail" {
  zone            = "example.cz"
  name            = "mail"
  type            = "A"
  ttl             = 300
  records         = ["192.0.2.26"]
  allow_overwrite = %t
}`, overwrite)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{Config: cfg(false), ExpectError: regexp.MustCompile(`already has 1 A row`)},
			{Config: cfg(true), Check: checkPublished(s, "mail", "A", "300 192.0.2.26")},
		},
	})
}

func TestAccValidation(t *testing.T) {
	s := wedosmock.New()
	defer s.Close()

	for name, tc := range map[string]struct{ body, err string }{
		"fqdn name":                {`name = "www.example.cz."` + "\n" + `type = "A"`, `Invalid record name`},
		"comment":                  {`name = "www"` + "\n" + `type = "A"` + "\n" + `author_comment = "ci-run #42"`, `Invalid author_comment`},
		"unsupported PTR":          {`name = "1"` + "\n" + `type = "PTR"`, `value must be one of`},
		"ttl too low":              {`name = "www"` + "\n" + `type = "A"` + "\n" + `ttl = 120`, `between 300 and 172800`},
		"ttl too high":             {`name = "www"` + "\n" + `type = "A"` + "\n" + `ttl = 200000`, `between 300 and 172800`},
		"alias not apex":           {`name = "cdn"` + "\n" + `type = "ALIAS"`, `only accepted at the zone apex`},
		"caa unquoted":             {`name = ""` + "\n" + `type = "CAA"` + "\n" + `records = ["0 issue letsencrypt.org"]`, `value quoted`},
		"cname two":                {`name = "www"` + "\n" + `type = "CNAME"` + "\n" + `records = ["a.example.net", "b.example.net"]`, `only one value`},
		"same after normalisation": {`name = ""` + "\n" + `type = "MX"` + "\n" + `records = ["10 mx.example.net", "10 MX.example.net."]`, `(?s)same record once\s+WEDOS\s+normalises`},
		"same address":             {`name = "v6"` + "\n" + `type = "AAAA"` + "\n" + `records = ["2001:db8::1", "2001:DB8:0::1"]`, `(?s)same record once\s+WEDOS\s+normalises`},
		"empty comment":            {`name = "www"` + "\n" + `type = "A"` + "\n" + `author_comment = ""`, `at least 1`},
	} {
		t.Run(name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{{
					Config: providerBlock(s) + fmt.Sprintf(`
resource "wedos_dns_record_set" "r" {
  zone = "example.cz"
  %s
}`, withDefaults(tc.body)),
					ExpectError: regexp.MustCompile(tc.err),
				}},
			})
		})
	}
}

// withDefaults adds ttl and records to a test body unless it sets them.
func withDefaults(body string) string {
	if !strings.Contains(body, "ttl") {
		body += "\n  ttl = 300"
	}
	if !strings.Contains(body, "records") {
		body += "\n  records = [\"192.0.2.1\"]"
	}
	return body
}

// Values WEDOS rewrites on store (trailing dots, CNAME case) must not cause a
// diff: the framework fails the step if the plan after apply is not empty.
func TestAccRecordSetNormalisation(t *testing.T) {
	s := wedosmock.New()
	defer s.Close()
	s.AddZone("example.cz")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config: providerBlock(s) + `
resource "wedos_dns_record_set" "mx" {
  zone    = "example.cz"
  name    = ""
  type    = "MX"
  ttl     = 3600
  records = ["10 MX1.Example.NET.", "20 mx2.example.net"]
}
resource "wedos_dns_record_set" "srv" {
  zone    = "example.cz"
  name    = "_sip._tcp"
  type    = "SRV"
  ttl     = 3600
  records = ["10 5 5060 sip.example.net."]
}
resource "wedos_dns_record_set" "cname" {
  zone    = "example.cz"
  name    = "docs"
  type    = "CNAME"
  ttl     = 300
  records = ["Docs.Example.NET."]
}
resource "wedos_dns_record_set" "caa" {
  zone    = "example.cz"
  name    = ""
  type    = "CAA"
  ttl     = 3600
  records = ["0 issue \"letsencrypt.org\""]
}
resource "wedos_dns_record_set" "wild" {
  zone    = "example.cz"
  name    = "*.apps"
  type    = "A"
  ttl     = 300
  records = ["192.0.2.50"]
}`,
			Check: resource.ComposeAggregateTestCheckFunc(
				checkPublished(s, "", "MX", "3600 10 mx1.example.net", "3600 20 mx2.example.net"),
				checkPublished(s, "docs", "CNAME", "300 docs.example.net"),
				resource.TestCheckTypeSetElemAttr("wedos_dns_record_set.cname", "records.*", "Docs.Example.NET."),
				checkPublished(s, "*.apps", "A", "300 192.0.2.50"),
			),
		}},
	})
}

func TestAccWaitForReload(t *testing.T) {
	s := wedosmock.New()
	defer s.Close()
	s.AddZone("example.cz")
	t.Setenv("WEDOS_RELOAD_POLL_INTERVAL", "5ms")

	cfg := func(extra string) string {
		return fmt.Sprintf(`
provider "wedos" {
  base_url        = %q
  client_id       = %q
  client_secret   = %q
  wait_for_reload = true
  %s
}
resource "wedos_dns_record_set" "www" {
  zone    = "example.cz"
  name    = "www"
  type    = "A"
  ttl     = 300
  records = ["192.0.2.1"]
}`, s.URL, s.ClientID, s.ClientSecret, extra)
	}
	var reads int
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config:      cfg(`reload_timeout = "soon"`),
				ExpectError: regexp.MustCompile(`Invalid reload_timeout`),
			},
			{
				PreConfig: func() {
					s.ReloadPolls = 3
					reads = s.Calls("GET /domains/{domain}")
				},
				Config: cfg(""),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkPublished(s, "www", "A", "300 192.0.2.1"),
					func(*terraform.State) error {
						if got := s.Calls("GET /domains/{domain}") - reads; got < 5 {
							return fmt.Errorf("want at least 5 zone reads while waiting, got %d", got)
						}
						return nil
					},
				),
			},
			{
				// A reload that never finishes is a warning, not a failed apply.
				PreConfig: func() { s.ReloadPolls = 1_000_000 },
				Config:    strings.Replace(cfg(`reload_timeout = "50ms"`), `"192.0.2.1"`, `"192.0.2.2"`, 1),
				Check:     checkPublished(s, "www", "A", "300 192.0.2.2"),
			},
		},
	})
}

// A create that fails after some values were written keeps them in state, so
// the resource is tainted and replaced on the next apply instead of the rows
// being orphaned and blocking it.
func TestAccRecordSetPartialCreate(t *testing.T) {
	s := wedosmock.New()
	defer s.Close()
	s.AddZone("example.cz")
	s.FailRdata = "192.0.2.99"

	cfg := providerBlock(s) + `
resource "wedos_dns_record_set" "www" {
  zone    = "example.cz"
  name    = "www"
  type    = "A"
  ttl     = 300
  records = ["192.0.2.1", "192.0.2.99"]
}`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config:      cfg,
				ExpectError: regexp.MustCompile(`Writing record set failed`),
			},
			{
				PreConfig: func() { s.FailRdata = "" },
				Config:    cfg,
				Check:     checkPublished(s, "www", "A", "300 192.0.2.1", "300 192.0.2.99"),
			},
		},
	})
}
