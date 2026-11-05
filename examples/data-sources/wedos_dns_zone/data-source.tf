data "wedos_dns_zone" "example" {
  name = "example.cz"
}

output "name_servers" {
  value = data.wedos_dns_zone.example.name_servers
}
