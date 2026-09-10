//go:build !unix

package config

// UseOpenat2 is unavailable on non-Unix platforms; always fall back to openat semantics.
func UseOpenat2() bool {
	return false
}
