# Changelog

All notable changes to this project are documented in this file. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/). The release workflow publishes the section whose
heading matches the tag as the release notes.

## 0.1.0

### Added

- Resource `wedos_dns_zone`: create, delete and import primary zones; outgoing AXFR allow-list.
- Resource `wedos_dns_record_set`: all values of one name and type, with import.
- Resource `wedos_dns_records_exclusive`: every record of a zone with one commit per apply; refuses
  to delete records outside the configuration on create.
- Data source `wedos_dns_zone`.
- Authentication with OAuth2 client credentials or an administration-issued bearer token.
- `wait_for_reload` to wait until the WEDOS API reports a commit processed.
- Plan-time validation of TTL range, ALIAS placement, CAA quoting and record names.
