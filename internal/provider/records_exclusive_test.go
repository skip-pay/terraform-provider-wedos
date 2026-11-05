package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/skip-pay/terraform-provider-wedos/internal/wedosmock"
)

const commitCalls = "POST /domains/{domain}/commit"

// commitsInStep asserts how many commits the step's apply issued.
type commitCounter struct {
	s    *wedosmock.Server
	base int
}

func (c *commitCounter) reset() { c.base = c.s.Calls(commitCalls) }

func (c *commitCounter) check(want int) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if got := c.s.Calls(commitCalls) - c.base; got != want {
			return fmt.Errorf("want %d commit(s) in this apply, got %d", want, got)
		}
		return nil
	}
}

func TestAccRecordsExclusive(t *testing.T) {
	s := wedosmock.New()
	defer s.Close()
	commits := &commitCounter{s: s}

	v1 := providerBlock(s) + `
resource "wedos_dns_zone" "z" {
  name = "example.cz"
}
resource "wedos_dns_records_exclusive" "z" {
  zone = wedos_dns_zone.z.name
  record_sets = [
    { name = "", type = "ALIAS", ttl = 300, records = ["d111111abcdef8.cloudfront.net"] },
    { name = "", type = "MX", ttl = 3600, records = ["10 mx1.example.net", "20 mx2.example.net"] },
    { name = "", type = "TXT", ttl = 3600, records = ["v=spf1 -all"], author_comment = "managed by terraform" },
    { name = "www", type = "CNAME", ttl = 300, records = ["d111111abcdef8.cloudfront.net"] },
    { name = "api", type = "A", ttl = 300, records = ["192.0.2.10", "192.0.2.11"] },
  ]
}`
	// Replace one api value, drop apex TXT, change MX TTL, add mail A, and turn
	// www from CNAME into A — the CNAME has to go before the A can be added.
	v2 := providerBlock(s) + `
resource "wedos_dns_zone" "z" {
  name = "example.cz"
}
resource "wedos_dns_records_exclusive" "z" {
  zone = wedos_dns_zone.z.name
  record_sets = [
    { name = "", type = "ALIAS", ttl = 300, records = ["d111111abcdef8.cloudfront.net"] },
    { name = "", type = "MX", ttl = 600, records = ["10 mx1.example.net", "20 mx2.example.net"] },
    { name = "www", type = "A", ttl = 300, records = ["192.0.2.80"] },
    { name = "api", type = "A", ttl = 300, records = ["192.0.2.10", "192.0.2.12"] },
    { name = "mail", type = "A", ttl = 300, records = ["192.0.2.25"] },
  ]
}`

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				PreConfig: commits.reset,
				Config:    v1,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("wedos_dns_records_exclusive.z", "id", "example.cz"),
					resource.TestCheckResourceAttr("wedos_dns_records_exclusive.z", "record_sets.#", "5"),
					checkPublished(s, "example.cz", "", "ALIAS", "300 d111111abcdef8.cloudfront.net"),
					checkPublished(s, "example.cz", "", "MX", "3600 10 mx1.example.net", "3600 20 mx2.example.net"),
					checkPublished(s, "example.cz", "", "TXT", "3600 v=spf1 -all"),
					checkPublished(s, "example.cz", "www", "CNAME", "300 d111111abcdef8.cloudfront.net"),
					checkPublished(s, "example.cz", "api", "A", "300 192.0.2.10", "300 192.0.2.11"),
					// One commit for the new zone, one for all its records.
					commits.check(2),
				),
			},
			{
				PreConfig: commits.reset,
				Config:    v2,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("wedos_dns_records_exclusive.z", "record_sets.#", "5"),
					checkPublished(s, "example.cz", "", "MX", "600 10 mx1.example.net", "600 20 mx2.example.net"),
					checkPublished(s, "example.cz", "", "TXT"),
					checkPublished(s, "example.cz", "www", "CNAME"),
					checkPublished(s, "example.cz", "www", "A", "300 192.0.2.80"),
					checkPublished(s, "example.cz", "api", "A", "300 192.0.2.10", "300 192.0.2.12"),
					checkPublished(s, "example.cz", "mail", "A", "300 192.0.2.25"),
					commits.check(1),
				),
			},
			{
				ResourceName:      "wedos_dns_records_exclusive.z",
				ImportState:       true,
				ImportStateId:     "example.cz",
				ImportStateVerify: true,
			},
			{
				// A record added outside Terraform is removed by the next apply.
				PreConfig: func() {
					s.AddRow("example.cz", wedosmock.Row{Name: "rogue", TTL: 60, Type: "TXT", Rdata: "added by hand"})
					commits.reset()
				},
				Config: v2,
				Check: resource.ComposeAggregateTestCheckFunc(
					checkPublished(s, "example.cz", "rogue", "TXT"),
					commits.check(1),
				),
			},
		},
	})
}

func TestAccRecordsExclusiveExistingZone(t *testing.T) {
	s := wedosmock.New()
	defer s.Close()
	s.AddZone("example.cz")
	s.AddRow("example.cz", wedosmock.Row{Name: "", TTL: 86400, Type: "NS", Rdata: "ns.wedos.cz."})
	s.AddRow("example.cz", wedosmock.Row{Name: "", TTL: 86400, Type: "NS", Rdata: "ns.wedos.net."})
	s.AddRow("example.cz", wedosmock.Row{Name: "legacy", TTL: 3600, Type: "A", Rdata: "192.0.2.99"})

	withoutLegacy := providerBlock(s) + `
resource "wedos_dns_records_exclusive" "z" {
  zone = "example.cz"
  record_sets = [
    { name = "www", type = "A", ttl = 300, records = ["192.0.2.80"] },
  ]
}`
	withLegacy := providerBlock(s) + `
resource "wedos_dns_records_exclusive" "z" {
  zone = "example.cz"
  record_sets = [
    { name = "www", type = "A", ttl = 300, records = ["192.0.2.80"] },
    { name = "legacy", type = "A", ttl = 3600, records = ["192.0.2.99"] },
  ]
}`

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		// Destroying the resource leaves the records in the zone.
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkPublished(s, "example.cz", "www", "A", "300 192.0.2.80"),
			checkPublished(s, "example.cz", "legacy", "A", "3600 192.0.2.99"),
		),
		Steps: []resource.TestStep{
			{
				Config:      withoutLegacy,
				ExpectError: regexp.MustCompile(`(?s)not in record_sets: legacy A\..*terraform\s+import`),
			},
			{
				Config: withLegacy,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("wedos_dns_records_exclusive.z", "record_sets.#", "2"),
					checkPublished(s, "example.cz", "www", "A", "300 192.0.2.80"),
					// Apex NS is neither in state nor touched.
					checkPublished(s, "example.cz", "", "NS", "86400 ns.wedos.cz.", "86400 ns.wedos.net."),
				),
			},
			{
				// Once managed, dropping a record set from the config deletes it.
				Config: withoutLegacy,
				Check: resource.ComposeAggregateTestCheckFunc(
					checkPublished(s, "example.cz", "legacy", "A"),
					checkPublished(s, "example.cz", "", "NS", "86400 ns.wedos.cz.", "86400 ns.wedos.net."),
				),
			},
			{
				Config: withLegacy,
			},
		},
	})
}

func TestAccRecordsExclusiveValidation(t *testing.T) {
	s := wedosmock.New()
	defer s.Close()

	for name, tc := range map[string]struct{ sets, err string }{
		"duplicate": {`
    { name = "www", type = "A", ttl = 300, records = ["192.0.2.1"] },
    { name = "www", type = "A", ttl = 600, records = ["192.0.2.2"] },`, `Duplicate record set`},
		"cname next to A": {`
    { name = "www", type = "A", ttl = 300, records = ["192.0.2.1"] },
    { name = "www", type = "CNAME", ttl = 300, records = ["host.example.net"] },`, `Conflicting record sets`},
		"alias next to A": {`
    { name = "", type = "A", ttl = 300, records = ["192.0.2.1"] },
    { name = "", type = "ALIAS", ttl = 300, records = ["d111111abcdef8.cloudfront.net"] },`, `Conflicting record sets`},
		"apex NS": {`
    { name = "", type = "NS", ttl = 3600, records = ["ns1.example.net."] },`, `Apex NS is not managed`},
	} {
		t.Run(name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{{
					Config: providerBlock(s) + fmt.Sprintf(`
resource "wedos_dns_records_exclusive" "z" {
  zone        = "example.cz"
  record_sets = [%s
  ]
}`, tc.sets),
					ExpectError: regexp.MustCompile(tc.err),
				}},
			})
		})
	}
}

// Replacing an apex A with an ALIAS in one apply: WEDOS rejects an ALIAS next
// to A/AAAA (2329), so the A has to be deleted before the ALIAS is added.
func TestAccRecordsExclusiveApexAToAlias(t *testing.T) {
	s := wedosmock.New()
	defer s.Close()
	s.AddZone("example.cz")

	cfg := func(apex string) string {
		return providerBlock(s) + fmt.Sprintf(`
resource "wedos_dns_records_exclusive" "z" {
  zone = "example.cz"
  record_sets = [
    %s,
    { name = "", type = "TXT", ttl = 3600, records = ["v=spf1 -all"] },
  ]
}`, apex)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: cfg(`{ name = "", type = "A", ttl = 300, records = ["203.0.113.80"] }`),
				Check:  checkPublished(s, "example.cz", "", "A", "300 203.0.113.80"),
			},
			{
				Config: cfg(`{ name = "", type = "ALIAS", ttl = 300, records = ["d111111abcdef8.cloudfront.net"] }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkPublished(s, "example.cz", "", "A"),
					checkPublished(s, "example.cz", "", "ALIAS", "300 d111111abcdef8.cloudfront.net"),
					checkPublished(s, "example.cz", "", "TXT", "3600 v=spf1 -all"),
				),
			},
		},
	})
}
