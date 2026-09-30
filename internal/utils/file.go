package utils

import (
	"fmt"
	"net/url"
	"strings"
)

// PathUnescape URL-unescapes path, falling back to decoding only %25.
func PathUnescape(path string) string {
	// try to use url.PathUnescape
	if unescaped, err := url.PathUnescape(path); err == nil {
		return unescaped
	}

	// unescape %
	unescapedPath := strings.ReplaceAll(path, "%25", "%")

	// add others

	return unescapedPath
}

// FormatSize renders bytes with binary units (KB = 1024 bytes).
func FormatSize(bytes int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
		tb = 1024 * gb
	)

	var size float64
	var unit string

	switch {
	case bytes >= tb:
		size = float64(bytes) / tb
		unit = "TB"
	case bytes >= gb:
		size = float64(bytes) / gb
		unit = "GB"
	case bytes >= mb:
		size = float64(bytes) / mb
		unit = "MB"
	case bytes >= kb:
		size = float64(bytes) / kb
		unit = "KB"
	default:
		size = float64(bytes)
		unit = "bytes"
	}

	// Format to 2 decimal places for larger units, no decimals for bytes
	if unit == "bytes" {
		return fmt.Sprintf("%.0f %s", size, unit)
	}
	return fmt.Sprintf("%.2f %s", size, unit)
}
