package server

import (
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// inlineHashes returns the CSP source of each inline element named tag in html.
func inlineHashes(html, tag string) []string {
	var hashes []string
	pattern := regexp.MustCompile(`(?is)<` + tag + `\b([^>]*)>(.*?)</` + tag + `\s*>`)
	for _, match := range pattern.FindAllStringSubmatch(html, -1) {
		if strings.Contains(strings.ToLower(match[1]), "src=") {
			continue
		}
		sum := sha256.Sum256([]byte(match[2]))
		hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	return hashes
}

func cspDirective(policy, name string) []string {
	for directive := range strings.SplitSeq(policy, ";") {
		if fields := strings.Fields(directive); len(fields) > 0 && fields[0] == name {
			return fields[1:]
		}
	}
	return nil
}

// The policy allows exactly the inline script and styles the templates render, so neither can drift from it.
func TestUIPagesContentSecurityPolicy(t *testing.T) {
	t.Parallel()
	pages, headers, _ := renderFixtures(t, "/")
	styles := map[string]bool{}
	for name, html := range pages {
		policy := headers[name].Get("Content-Security-Policy")
		if policy == "" {
			t.Errorf("%s: no Content-Security-Policy header", name)
			continue
		}
		scripts := inlineHashes(html, "script")
		if len(scripts) != 1 {
			t.Errorf("%s: %d inline scripts, want only the theme script", name, len(scripts))
			continue
		}
		if got, want := cspDirective(policy, "script-src"), []string{"'self'", scripts[0]}; !slices.Equal(got, want) {
			t.Errorf("%s: script-src = %q, want %q", name, got, want)
		}
		for _, hash := range inlineHashes(html, "style") {
			styles[hash] = true
			if !slices.Contains(cspDirective(policy, "style-src"), hash) {
				t.Errorf("%s: style-src lacks %s for its inline style", name, hash)
			}
		}
	}
	for _, source := range cspDirective(headers["index"].Get("Content-Security-Policy"), "style-src") {
		if source != "'self'" && !styles[source] {
			t.Errorf("style-src allows %s, which no page renders", source)
		}
	}
}
