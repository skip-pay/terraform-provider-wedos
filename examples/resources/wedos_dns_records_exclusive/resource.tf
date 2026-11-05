# Owns every record in the zone except the apex NS records. Record sets that
# are not listed here are deleted on apply.
resource "wedos_dns_records_exclusive" "example" {
  zone = "example.cz"

  record_sets = [
    { name = "", type = "ALIAS", ttl = 300, records = ["d111111abcdef8.cloudfront.net"] },
    { name = "", type = "MX", ttl = 3600, records = ["10 mx1.example.net"] },
    { name = "", type = "TXT", ttl = 3600, records = ["v=spf1 include:_spf.example.net -all"] },
    { name = "www", type = "CNAME", ttl = 300, records = ["d111111abcdef8.cloudfront.net"] },
  ]
}
