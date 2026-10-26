package server

import (
	"fmt"
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

func reportBanned(t *testing.T, rel string, banned []string, why string) {
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
				t.Errorf("%s:%d: %s is banned; %s", rel, line, b, why)
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
	configBanned := []string{"--ignore", "--allow", " -A ", "--disable", `"ignorePatterns"`, `"exclude"`}
	why := "fix the finding, or turn the rule off in config with a reason"
	files := suppressionScope(t)
	if len(files) == 0 {
		t.Fatal("scanned no files")
	}
	for _, rel := range files {
		if strings.Contains(rel, "/") {
			reportBanned(t, rel, banned, why)
		} else {
			reportBanned(t, rel, slices.Concat(banned, configBanned), why)
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

const jsAllowlist = "pkg/server/ui/js-allowlist.txt"

func TestThemeScriptStaysStatic(t *testing.T) {
	t.Parallel()
	const rel = "pkg/server/templates/theme_script.html"
	src, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatal(err)
	}
	if problem := themeScriptProblem(string(src)); problem != "" {
		t.Errorf("%s: %s; it runs before first paint, so it holds the theme logic and nothing else", rel, problem)
	}
	if listed, _ := readJSAllowlist(t); !slices.Contains(listed, rel) {
		t.Errorf("%s is not in %s", rel, jsAllowlist)
	}
}

// themeScriptProblem says why src is not a single static inline script wrapped in its define block, or "".
func themeScriptProblem(src string) string {
	body, defined := strings.CutPrefix(strings.TrimSpace(src), `{{ define "theme_script" }}`)
	body, ended := strings.CutSuffix(body, "{{ end }}")
	if !defined || !ended {
		return `it is not one {{ define "theme_script" }} ... {{ end }} block`
	}
	if strings.Contains(body, "{{") {
		return "it holds a template action"
	}
	scripts := regexp.MustCompile(`(?is)<script\b`).FindAllStringIndex(body, -1)
	if len(scripts) != 1 {
		return fmt.Sprintf("it holds %d <script> elements, want 1", len(scripts))
	}
	script := regexp.MustCompile(`(?is)^\s*<script>(.*?)</script\s*>\s*$`).FindStringSubmatch(body)
	if script == nil || strings.TrimSpace(script[1]) == "" {
		return "it is not one bare <script> element with code"
	}
	if hasInlineScript(strings.Replace(body, script[0], "", 1)) {
		return "it holds a handler attribute or javascript: URL"
	}
	return ""
}

func TestThemeScriptProblem(t *testing.T) {
	t.Parallel()
	wrap := func(body string) string { return "{{ define \"theme_script\" }}\n" + body + "\n{{ end }}\n" }
	for src, want := range map[string]bool{
		wrap("<script>theme()</script>"):                          true,
		wrap("<script>theme()</script>\n<script>more()</script>"): false,
		wrap(`<script>window.urlBase = "{{.URLBase}}"</script>`):  false,
		wrap(`<script>theme()</script><div onclick="x()"></div>`): false,
		wrap(`<script src="a.js"></script>`):                      false,
		wrap(`<script type="module">theme()</script>`):            false,
		wrap(""):                              false,
		"<script>theme()</script>":            false,
		"<script>theme()</script>\n{{ end }}": false,
		wrap("<script>theme()</script>") + `{{ define "x" }}{{ end }}`: false,
	} {
		if got := themeScriptProblem(src) == ""; got != want {
			t.Errorf("themeScriptProblem(%q) = %q, want ok = %v", src, themeScriptProblem(src), want)
		}
	}
}

func parseJSAllowlist(raw string) ([]string, []string) {
	var paths, problems []string
	line := 0
	for text := range strings.Lines(raw) {
		line++
		entry := strings.TrimSpace(text)
		if entry == "" {
			continue
		}
		path, _, ok := strings.Cut(entry, ": ")
		if !ok || strings.TrimSpace(path) == "" {
			problems = append(problems, fmt.Sprintf("%s:%d: %q is not \"<path>: <reason>\"", jsAllowlist, line, entry))
			continue
		}
		paths = append(paths, strings.TrimSpace(path))
	}
	return paths, problems
}

func readJSAllowlist(t *testing.T) ([]string, []string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, jsAllowlist))
	if err != nil {
		t.Fatal(err)
	}
	return parseJSAllowlist(string(raw))
}

func TestParseJSAllowlist(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]int{
		"a.js: legacy\n\nb.html: early theme\n": 0,
		"a.js\n":                                1,
		"a.js:\n":                               1,
		"a.js:   \n":                            1,
		": reason\n":                            1,
		"a.js:reason\n":                         1,
		"a.js: ok\nb.js\n":                      1,
	} {
		if _, problems := parseJSAllowlist(raw); len(problems) != want {
			t.Errorf("parseJSAllowlist(%q) problems = %q, want %d", raw, problems, want)
		}
	}
}

func TestJSAllowlistLinesHaveReasons(t *testing.T) {
	t.Parallel()
	paths, problems := readJSAllowlist(t)
	for _, p := range problems {
		t.Error(p)
	}
	if len(paths) == 0 {
		t.Fatalf("%s lists nothing", jsAllowlist)
	}
}

func TestNoHTMLStringAPIs(t *testing.T) {
	t.Parallel()
	banned := []string{
		"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "createContextualFragment",
		"setHTMLUnsafe", "parseHTMLUnsafe", "DOMParser", "srcdoc",
	}
	files := repoFiles(t, "pkg/server/ui", func(rel string) bool {
		return filepath.Ext(rel) == ".ts" && !strings.HasSuffix(rel, ".test.ts") &&
			rel != "pkg/server/ui/lib/testing/fixture.ts"
	})
	if len(files) == 0 {
		t.Fatal("scanned no files")
	}
	for _, rel := range files {
		reportBanned(t, rel, banned, "build markup from a <template> and set text with textContent")
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

	raw, err := os.ReadFile(filepath.Join(repoRoot, jsAllowlist))
	if err != nil {
		t.Fatal(err)
	}
	const themeOnly = "pkg/server/templates/theme_script.html: early theme script, runs before first paint (cannot be a module)\n"
	if string(raw) != themeOnly {
		t.Errorf("%s = %q, want only the theme script line %q", jsAllowlist, raw, themeOnly)
	}
	listed, _ := parseJSAllowlist(string(raw))
	for _, rel := range found {
		if !slices.Contains(listed, rel) {
			t.Errorf("%s: new JavaScript; write it in TypeScript under pkg/server/ui", rel)
		}
	}
	for _, rel := range listed {
		if !slices.Contains(found, rel) {
			t.Errorf("%s: listed in %s but no longer JavaScript; remove the line", rel, jsAllowlist)
		}
	}
}
