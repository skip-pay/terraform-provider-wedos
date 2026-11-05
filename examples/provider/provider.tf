terraform {
  required_providers {
    wedos = {
      source = "skip-pay/wedos"
    }
  }
}

# Credentials of a WEDOS REST API client with the "wdns" scope. Prefer the
# environment variables WEDOS_CLIENT_ID and WEDOS_CLIENT_SECRET, or
# WEDOS_ACCESS_TOKEN for a token generated in the WEDOS administration.
provider "wedos" {}
