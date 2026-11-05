//go:build generate

package tools

import (
	_ "github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs"
)

// Format the examples used in the documentation.
//go:generate terraform fmt -recursive ../examples/

// Generate docs/ from the provider schema, examples/ and templates/.
//go:generate go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs generate --provider-dir .. --provider-name wedos
