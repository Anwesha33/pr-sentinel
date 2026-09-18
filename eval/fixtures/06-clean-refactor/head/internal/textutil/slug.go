package textutil

import "strings"

// Slug turns a title into a URL-safe slug.
func Slug(title string) string {
	return SlugWithMax(title, 0)
}

// SlugWithMax is Slug with an optional maximum length. A maxLen of 0 means no
// limit. Truncation never leaves a trailing dash.
func SlugWithMax(title string, maxLen int) string {
	lower := strings.ToLower(strings.TrimSpace(title))
	var b strings.Builder
	lastDash := false
	for _, r := range lower {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.TrimSuffix(b.String(), "-")
	if maxLen > 0 && len(out) > maxLen {
		out = strings.TrimSuffix(out[:maxLen], "-")
	}
	return out
}
