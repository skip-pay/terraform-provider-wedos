resource "wedos_dns_record_set" "www" {
  zone    = "example.cz"
  name    = "www"
  type    = "A"
  ttl     = 300
  records = ["192.0.2.10", "192.0.2.11"]
}

# Apex records use an empty name.
resource "wedos_dns_record_set" "mx" {
  zone    = "example.cz"
  name    = ""
  type    = "MX"
  ttl     = 3600
  records = ["10 mx1.example.net", "20 mx2.example.net"]
}

# ALIAS points the zone apex at a host name, e.g. a CDN distribution.
resource "wedos_dns_record_set" "apex" {
  zone    = "example.cz"
  name    = ""
  type    = "ALIAS"
  ttl     = 300
  records = ["d111111abcdef8.cloudfront.net"]
}
