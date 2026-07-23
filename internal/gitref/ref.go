// Package gitref validates immutable Git reference names without invoking Git.
package gitref

import "strings"

// Valid accepts fully-qualified references that satisfy Git's refname rules.
func Valid(value string) bool {
	if !strings.HasPrefix(value, "refs/") || value == "refs/" || value != strings.TrimSpace(value) ||
		strings.HasSuffix(value, "/") || strings.Contains(value, "//") || strings.Contains(value, "..") ||
		strings.Contains(value, "@{") || strings.ContainsAny(value, "~^:?*[\\") {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
		for _, runeValue := range component {
			if runeValue <= 0x20 || runeValue == 0x7f {
				return false
			}
		}
	}
	return true
}
