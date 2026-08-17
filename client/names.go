package client

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/gosimple/slug"
)

// SanitizeProjectName converts a string to a valid Incus project name.
// Replaces underscores with hyphens and removes special characters via slug.
func SanitizeProjectName(name string) string {
	safe := slug.Make(name)
	safe = strings.ReplaceAll(safe, "_", "-")
	return safe
}

// SanitizeIncusName converts a string to a valid Incus instance name.
// Converts to lowercase, replaces special chars and underscores with hyphens.
// Names exceeding maxLength are replaced with a 32-char hex hash for DNS
// compatibility, so -1 means the Incus limit and 0 hashes every name.
func SanitizeIncusName(name string, maxLength int) string {
	if maxLength == -1 {
		maxLength = MaxIncusNameLen
	}

	// slug.Make converts to lowercase, replaces special chars with hyphens
	// but keeps underscores, so we replace them explicitly
	safe := slug.Make(name)
	safe = strings.ReplaceAll(safe, "_", "-")

	if len(safe) > maxLength {
		// Fall back to hash for very long names
		sha256sum := sha256.Sum256([]byte(name))
		safe = hex.EncodeToString(sha256sum[:16]) // 32 hex chars
	}

	return safe
}
