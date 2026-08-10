package client

// WellKnownRegistries maps well-known OCI registry domains to their server URLs.
// An image from one of these resolves without an `incus remote add` step.
var WellKnownRegistries = map[string]string{
	"ghcr.io":             "https://ghcr.io",
	"docker.io":           "https://docker.io",
	"mcr.microsoft.com":   "https://mcr.microsoft.com",
	"quay.io":             "https://quay.io",
	"registry.gitlab.com": "https://registry.gitlab.com",
	"codeberg.org":        "https://codeberg.org",
}
