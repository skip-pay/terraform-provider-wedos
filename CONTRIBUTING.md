# Contributing

Issues and pull requests are welcome. For a larger change, open an issue first so the approach can
be agreed before you write it.

## Requirements

- Go, the version in `go.mod`
- Terraform (for acceptance tests and `make generate`)
- [golangci-lint](https://golangci-lint.run/) for `make lint`

No WEDOS account is needed for development: tests run against an in-memory fake of the API.

## Layout

| Path | Contents |
| --- | --- |
| `main.go` | provider server, address `registry.terraform.io/skip-pay/wedos` |
| `internal/client` | WEDOS REST API client: OAuth2 token source, response envelope, error classes, DNS endpoints |
| `internal/provider` | provider, resources, data source |
| `internal/provider/zone_store.go` | per-zone lock and row cache shared by all resources of one run |
| `internal/provider/sync.go` | plans and orders row writes for a record set or a whole zone |
| `internal/wedosmock` | in-memory fake of the WEDOS DNS endpoints used by the tests |
| `cmd/wedos-mock` | runs the fake on a local port |
| `examples/`, `templates/` | sources of the generated documentation |
| `docs/` | generated documentation; do not edit by hand |

## How the provider maps to the API

WEDOS stores one row per record value with a server-assigned ID. The resources expose record sets
(all values of one name and type) and never store row IDs: every operation resolves rows from the
zone's row list, which is fetched once per run and updated in place on writes. Writes are planned
by `sync.go`: changed values are rewritten in place, additions run before deletions, and deletions
that would block an addition (CNAME next to other types, ALIAS next to A/AAAA) run first. Each
resource commits the zone after writing; `wedos_dns_records_exclusive` commits once per apply.

The API returns HTTP 200 for every response and carries the result in the `code` field. Numbers
may arrive as strings. Reads are retried on transient codes; writes are never retried, because the
API has no idempotency keys.

## Tests

```bash
make test       # unit tests
make testacc    # acceptance tests: real Terraform against internal/wedosmock
```

The fake models the behaviour of the live API, not only its documentation. When you change the
provider because the live API behaves in a certain way, model that behaviour in
`internal/wedosmock` and cover it with a test, so it does not regress.

Changes that depend on API behaviour should also be checked against a real WEDOS account on a
zone you can delete. Point the provider at it with `WEDOS_ACCESS_TOKEN` (or client credentials)
and a local build through `dev_overrides`.

## Documentation

Documentation in `docs/` is generated from the schema descriptions, `examples/` and `templates/`
by [tfplugindocs](https://github.com/hashicorp/terraform-plugin-docs). After changing any of them
run `make generate` and commit the result; CI fails when `docs/` is out of date.

## Pull requests

- One logical change per pull request.
- `make fmt lint test testacc generate` pass.
- `CHANGELOG.md` has an entry under the next version.

## Releasing (maintainers)

1. Move the changes in `CHANGELOG.md` under a heading `## X.Y.Z`.
2. Push a tag `vX.Y.Z` on `main`.
3. The release workflow checks that the changelog section exists, runs the tests, builds the
   binaries with GoReleaser, signs `SHA256SUMS` with the project GPG key and creates the GitHub
   release. The Terraform and OpenTofu registries pick up the release from there.

The workflow runs in the `release` environment, which holds the secrets `GPG_PRIVATE_KEY` and
`PASSPHRASE` and requires a maintainer's approval. Tags `v*` can only be created by maintainers.
The public part of the key is registered with both registries. Published versions are never
replaced; fix forward with a new version.
