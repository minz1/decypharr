package utils

import "slices"

// RemoveItem returns a copy of s without any of values.
func RemoveItem[S ~[]E, E comparable](s S, values ...E) S {
	result := make(S, 0, len(s))
	for _, item := range s {
		if !slices.Contains(values, item) {
			result = append(result, item)
		}
	}
	return result
}

// Mask hides the middle of a secret for display, keeping a short prefix and
// suffix only when the secret is long enough to spare them.
func Mask(text string) string {
	const (
		longSecret  = 12 // show 8 leading and 4 trailing characters
		shortSecret = 8  // show 4 leading and 2 trailing characters
	)
	switch {
	case len(text) > longSecret:
		return text[:8] + "****" + text[len(text)-4:]
	case len(text) > shortSecret:
		return text[:4] + "****" + text[len(text)-2:]
	default:
		return "****"
	}
}
