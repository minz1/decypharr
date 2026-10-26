package server

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const repoRoot = "../.."

func repoFiles(t *testing.T, dir string, keep func(rel string) bool) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(filepath.Join(repoRoot, dir), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(repoRoot, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			hidden := rel != "." && strings.HasPrefix(d.Name(), ".")
			if hidden || d.Name() == "node_modules" || rel == "docs" || rel == "pkg/server/assets/build" {
				return filepath.SkipDir
			}
			return nil
		}
		if keep(rel) {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func suppressionScope(t *testing.T) []string {
	t.Helper()
	all := func(string) bool { return true }
	files := slices.Concat(
		repoFiles(t, "pkg/server/ui", all),
		repoFiles(t, "pkg/server/templates", all),
		repoFiles(t, "scripts", all),
	)
	for _, pattern := range []string{"*.json", "*.ts", ".djlintrc"} {
		root, err := filepath.Glob(filepath.Join(repoRoot, pattern))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range root {
			files = append(files, filepath.Base(p))
		}
	}
	return files
}

func reportBanned(t *testing.T, rel string, banned []string) {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatal(err)
	}
	line := 0
	for text := range strings.Lines(string(src)) {
		line++
		for _, b := range banned {
			if strings.Contains(text, b) {
				t.Errorf("%s:%d: %s is banned; fix the finding, or turn the rule off in config with a reason",
					rel, line, b)
			}
		}
	}
}

func TestNoSuppressions(t *testing.T) {
	t.Parallel()
	banned := []string{
		"oxlint-disable", "eslint-disable", "@ts-ignore", "@ts-expect-error", "@ts-nocheck",
		"djlint:off", "ignorePatterns",
	}
	// Only in the root configs: CLI flags and keys that skip files or rules.
	configBanned := []string{"--ignore", "--allow", " -A ", "--disable", `"ignore"`, `"exclude"`}
	files := suppressionScope(t)
	if len(files) == 0 {
		t.Fatal("scanned no files")
	}
	for _, rel := range files {
		if strings.Contains(rel, "/") {
			reportBanned(t, rel, banned)
		} else {
			reportBanned(t, rel, slices.Concat(banned, configBanned))
		}
	}
	for _, ignoreFile := range []string{".eslintignore", ".oxlintignore", ".prettierignore", ".oxfmtignore"} {
		if _, err := os.Stat(filepath.Join(repoRoot, ignoreFile)); err == nil {
			t.Errorf("%s: ignore files are banned; fix the finding, or turn the rule off in config with a reason",
				ignoreFile)
		}
	}
}

// hasInlineScript reports a <script> that carries code (no src, not a data block), an on* handler or a javascript: URL.
func hasInlineScript(html string) bool {
	handler := regexp.MustCompile(`(?is)<[a-z][^>]*\s(on[a-z]+\s*=|href\s*=\s*["']?\s*javascript:)`)
	if handler.MatchString(html) {
		return true
	}
	tag := regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script\s*>`)
	src := regexp.MustCompile(`(?i)(?:^|\s)src\s*=`)
	data := regexp.MustCompile(`(?i)(?:^|\s)type\s*=\s*["']?(application/json|speculationrules)\b`)
	for _, m := range tag.FindAllStringSubmatch(html, -1) {
		attrs, body := m[1], m[2]
		if src.MatchString(attrs) || data.MatchString(attrs) {
			continue
		}
		if strings.TrimSpace(body) != "" {
			return true
		}
	}
	return false
}

func TestHasInlineScript(t *testing.T) {
	t.Parallel()
	for html, want := range map[string]bool{
		`<script>x()</script>`:                      true,
		`<SCRIPT>x()</SCRIPT>`:                      true,
		"<script\n  type=\"module\"\n>x()</script>": true,
		`<script>x()</script >`:                     true,
		`<script data-src="a">x()</script>`:         true,
		`<script type="text/javascript" data-type="application/json">x()</script>`: true,
		`<button class="btn" onclick="x()">Go</button>`:                            true,
		`<a href="javascript:x()">Go</a>`:                                          true,
		`<script src="a.js"></script>`:                                             false,
		`<script type="application/json">{"a":1}</script>`:                         false,
		`<script type="speculationrules">{}</script>`:                              false,
		`<script>  </script>`:                                                      false,
		`<div data-onclick="x">someone = 1</div>`:                                  false,
	} {
		if got := hasInlineScript(html); got != want {
			t.Errorf("hasInlineScript(%q) = %v, want %v", html, got, want)
		}
	}
}

func TestLegacyJSRatchet(t *testing.T) {
	t.Parallel()
	isJS := func(rel string) bool {
		return slices.Contains([]string{".js", ".mjs", ".cjs"}, filepath.Ext(rel))
	}
	found := repoFiles(t, ".", isJS)
	for _, rel := range repoFiles(t, "pkg/server/templates", func(rel string) bool { return filepath.Ext(rel) == ".html" }) {
		src, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatal(err)
		}
		if hasInlineScript(string(src)) {
			found = append(found, rel)
		}
	}

	raw, err := os.ReadFile(filepath.Join(repoRoot, "pkg/server/ui/legacy-js.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for line := range strings.Lines(string(raw)) {
		if entry := strings.TrimSpace(line); entry != "" {
			listed = append(listed, entry)
		}
	}
	for _, rel := range found {
		if !slices.Contains(listed, rel) {
			t.Errorf("%s: new JavaScript; write it in TypeScript under pkg/server/ui", rel)
		}
	}
	for _, rel := range listed {
		if !slices.Contains(found, rel) {
			t.Errorf("%s: listed in pkg/server/ui/legacy-js.txt but no longer JavaScript; remove the line", rel)
		}
	}
}
