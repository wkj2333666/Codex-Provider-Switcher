// Package provider validates model provider identifiers accepted by the switcher.
package provider

import "regexp"

var namePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Valid reports whether name is safe to use as a Codex model provider id.
func Valid(name string) bool {
	return namePattern.MatchString(name)
}
