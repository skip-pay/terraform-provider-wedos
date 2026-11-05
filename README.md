# Terraform Provider for WEDOS DNS

Manage [WEDOS DNS](https://www.wedos.cz/) zones and records with Terraform or OpenTofu, through the
[WEDOS HTTP REST API v2](https://kb.wedos.global/api-dns/).

Maintained by [Skip Pay](https://github.com/skip-pay). Not affiliated with or endorsed by
WEDOS Internet, a.s.

> The WEDOS REST API is described by WEDOS as still in development. This provider follows the
> behaviour of the live API, which differs from the documentation in places; see
> [WEDOS API behaviour](docs/guides/wedos-api-behaviour.md).

## Features

| Type | Name | Manages |
| --- | --- | --- |
| Resource | `wedos_dns_zone` | a primary zone and its outgoing AXFR allow-list |
| Resource | `wedos_dns_record_set` | all values of one name and type |
| Resource | `wedos_dns_records_exclusive` | every record of a zone, with one commit per apply |
| Data source | `wedos_dns_zone` | an existing zone, its name servers and serial |

Supported record types: A, AAAA, ALIAS, CAA, CNAME, DNAME, HTTPS, MX, NAPTR, NS, SRV, SSHFP,
TLSA, TXT.

## Requirements

- Terraform 1.13 or later, or OpenTofu
- A WEDOS REST API client with the `wdns` scope, created in the WEDOS customer administration

## Usage

```hcl
terraform {
  required_providers {
    wedos = {
      source = "skip-pay/wedos"
    }
  }
}

# WEDOS_CLIENT_ID and WEDOS_CLIENT_SECRET, or WEDOS_ACCESS_TOKEN, in the environment.
provider "wedos" {}

resource "wedos_dns_zone" "example" {
  name = "example.cz"
}

resource "wedos_dns_record_set" "www" {
  zone    = wedos_dns_zone.example.name
  name    = "www"
  type    = "A"
  ttl     = 300
  records = ["192.0.2.10", "192.0.2.11"]
}
```

Full documentation: [docs/](docs/index.md), also published on the Terraform and OpenTofu
registries.

## Development

```bash
make build      # compile
make test       # unit tests
make testacc    # acceptance tests against the in-memory WEDOS API fake
make generate   # regenerate docs/ from the schema, examples/ and templates/
make mock       # run the fake API locally for manual terraform runs
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the code layout and the release process.

## Security

Report vulnerabilities privately, see [SECURITY.md](SECURITY.md).

## License

[Mozilla Public License 2.0](LICENSE). Copyright Skip Pay.
