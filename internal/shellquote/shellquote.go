// Package shellquote formats untrusted values as POSIX shell words.
package shellquote

import "strings"

// Quote returns value unchanged when it contains only shell-safe characters,
// and otherwise wraps it in single quotes with embedded quotes escaped.
func Quote(value string) string {
	if value != "" {
		safe := true
		for _, r := range value {
			if !isSafe(r) {
				safe = false
				break
			}
		}
		if safe {
			return value
		}
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func isSafe(r rune) bool {
	return r >= 'a' && r <= 'z' ||
		r >= 'A' && r <= 'Z' ||
		r >= '0' && r <= '9' ||
		strings.ContainsRune("_@%+=:,./-", r)
}
