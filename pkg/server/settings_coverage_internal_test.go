package server

import (
	"encoding"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
)

type settingsSection struct {
	populate []string
	collect  string
}

type settingsCoverage struct {
	sections  map[string]settingsSection
	listNames [][2]string
	// Keys ending in "." match a whole subtree; the value is the text the page must contain.
	aliases    map[string]func(path string) string
	exclusions map[string]string
}

func newSettingsCoverage() settingsCoverage {
	return settingsCoverage{
		sections: map[string]settingsSection{
			"": {
				[]string{"populateForm", "populateGeneralSettings", "populateDownloadSettings"},
				"collectFormData",
			},
			"hearsay.":               {[]string{"populateHearsaySettings"}, "collectHearsayConfig"},
			"repair.":                {[]string{"populateRepairSettings"}, "collectRepairConfig"},
			"prune.":                 {[]string{"populatePruneSettings"}, "collectPruneConfig"},
			"notifications.":         {[]string{"populateNotificationSettings"}, "collectNotificationsConfig"},
			"mount.":                 {[]string{"populateMountSettings"}, "collectMountConfig"},
			"mount.rclone.":          {[]string{"populateRcloneSettings"}, "collectRcloneConfig"},
			"mount.dfs.":             {[]string{"populateDFSSettings"}, "collectDFSConfig"},
			"mount.external_rclone.": {[]string{"populateExternalRcloneSettings"}, "collectExternalRclone"},
			"nfs.":                   {[]string{"populateNFSSettings"}, "collectNFSConfig"},
			"smb.":                   {[]string{"populateSMBSettings"}, "collectSMBConfig"},
			"share_cache.":           {[]string{"populateShareCacheSettings"}, "collectShareCacheConfig"},
			"usenet.":                {[]string{"populateUsenetSettings"}, "collectUsenetConfig"},
			"debrids[].":             {nil, "collectDebridConfigs"},
			"arrs[].":                {nil, "collectArrConfigs"},
			"usenet.providers[].":    {nil, "collectUsenetConfig"},
		},
		listNames: [][2]string{
			{"debrids[].", "debrid[${index}]."},
			{"arrs[].", "arr[${index}]."},
			{"usenet.providers[].", "usenet.providers[${index}]."},
		},
		aliases: map[string]func(path string) string{
			"strm.":                  func(p string) string { return ` name="strm_` + strings.TrimPrefix(p, "strm.") + `"` },
			"hearsay.disabled":       func(string) string { return `name="hearsay.enabled"` },
			"notifications.events":   func(string) string { return `name="notifications.events[]"` },
			"repair.arrs":            func(string) string { return `id="repair.arrs"` },
			"virtual_folders[].":     func(string) string { return "collectVirtualFolders(" },
			"queue_cleanup.rules[].": func(string) string { return "collectQueueCleanup(" },
		},
		exclusions: map[string]string{
			"qbittorrent.":              "deprecated",
			"rclone.":                   "deprecated",
			"discord_webhook_url":       "deprecated",
			"callback_url":              "deprecated",
			"custom_folders":            "deprecated",
			"debrids[].folder":          "deprecated",
			"debrids[].folder_naming":   "deprecated",
			"debrids[].rc_url":          "deprecated",
			"debrids[].rc_user":         "deprecated",
			"debrids[].rc_pass":         "deprecated",
			"debrids[].rc_refresh_dirs": "deprecated",
			"debrids[].directories":     "deprecated",
			"use_auth":                  "managed elsewhere",
			"strm.secret":               "managed elsewhere",
			"mount.rclone.enabled":      "not read by the code",
			"mount.rclone.mount_path":   "not read by the code",
			"skip_auto_move":            "not read by the code",
			"debrids[].workers":         "not read by the code",
		},
	}
}

type configLeaf struct {
	path    string
	numeric bool
}

func isJSONNumber(t reflect.Type) bool {
	p := reflect.PointerTo(t)
	if p.Implements(reflect.TypeFor[json.Unmarshaler]()) || p.Implements(reflect.TypeFor[encoding.TextUnmarshaler]()) {
		return false
	}
	return slices.Contains([]reflect.Kind{
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64,
	}, t.Kind())
}

func configPaths(t reflect.Type, prefix string, out *[]configLeaf) error {
	for f := range t.Fields() {
		if !f.IsExported() {
			continue
		}
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if tag == "-" {
			continue
		}
		if tag == "" {
			return fmt.Errorf("%s%s: exported field without a json name", prefix, f.Name)
		}
		p := prefix + tag
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		var err error
		switch {
		case ft.Kind() == reflect.Struct:
			err = configPaths(ft, p+".", out)
		case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct:
			err = configPaths(ft.Elem(), p+"[].", out)
		default:
			*out = append(*out, configLeaf{path: p, numeric: isJSONNumber(ft)})
		}
		if err != nil {
			return err
		}
	}
	return nil
}

var jsMethodEnd = regexp.MustCompile(`\n    }\n|\n    (async )?[a-zA-Z]+\(.*\) \{\n`)

func jsMethodBody(src, name string) (string, bool) {
	start := strings.Index(src, "\n    "+name+"(")
	if start < 0 {
		return "", false
	}
	body := src[start+1:]
	first := strings.Index(body, "\n")
	if loc := jsMethodEnd.FindStringIndex(body[first:]); loc != nil {
		return body[:first+loc[0]], true
	}
	return body, true
}

func matchKey[V any](m map[string]V, path string) (string, bool) {
	for k := range m {
		if k == path || (strings.HasSuffix(k, ".") && strings.HasPrefix(path, k)) {
			return k, true
		}
	}
	return "", false
}

type settingsPage struct{ html, js, build string }

func readSettingsPage(t *testing.T) settingsPage {
	t.Helper()
	read := func(name string) string {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return regexp.MustCompile(`\$\{[^}]*\}`).ReplaceAllLiteralString(string(b), "${index}")
	}
	return settingsPage{
		html:  read("templates/config.html"),
		js:    read("assets/js/config.js"),
		build: read("assets/build/js/config.js"),
	}
}

func (p settingsPage) shows(text string) bool {
	return strings.Contains(p.html, text) || (strings.Contains(p.js, text) && strings.Contains(p.build, text))
}

func (p settingsPage) mentions(text string) bool {
	return strings.Contains(p.html, text) || strings.Contains(p.js, text) || strings.Contains(p.build, text)
}

func (c settingsCoverage) sortedPrefixes(t *testing.T, js string) []string {
	t.Helper()
	prefixes := make([]string, 0, len(c.sections))
	for p, s := range c.sections {
		prefixes = append(prefixes, p)
		for _, m := range append(s.populate, s.collect) {
			if _, ok := jsMethodBody(js, m); !ok {
				t.Errorf("%s: method %s not in config.js", p, m)
			}
		}
	}
	sort.Slice(prefixes, func(i, j int) bool { return len(prefixes[i]) > len(prefixes[j]) })
	return prefixes
}

func (c settingsCoverage) formRef(path string) string {
	for _, l := range c.listNames {
		path = strings.Replace(path, l[0], l[1], 1)
	}
	return ` name="` + path + `"`
}

func controlTag(text, ref string) string {
	i := strings.Index(text, ref)
	if i < 0 {
		return ""
	}
	start := strings.LastIndex(text[:i], "<")
	end := strings.Index(text[i:], ">")
	if start < 0 || end < 0 {
		return ""
	}
	return text[start : i+end]
}

// sendsNumber: a JSON string for a numeric field fails the whole save with a 400.
func sendsNumber(page settingsPage, collectBody, leaf, ref string) bool {
	q := regexp.QuoteMeta(leaf)
	line := regexp.MustCompile(`(?m)^.*(\b` + q + `['"]?\s*:|\.` + q + `\s*=).*$`).FindString(collectBody)
	for _, numeric := range []string{"parseInt(", "parseFloat(", "Number", "Math."} {
		if strings.Contains(line, numeric) {
			return true
		}
	}
	tag := controlTag(page.html, ref) + controlTag(page.js, ref)
	return strings.Contains(line, "getElementValue(") && strings.Contains(tag, `type="number"`)
}

func (c settingsCoverage) checkControl(t *testing.T, page settingsPage, prefixes []string, leafInfo configLeaf) {
	t.Helper()
	path := leafInfo.path
	ref := c.formRef(path)
	if !page.shows(ref) {
		if strings.Contains(page.js, ref) {
			t.Errorf("%s:%s in config.js but not in the build; run node scripts/minify-js.js", path, ref)
		} else {
			t.Errorf("%s: no control%s", path, ref)
		}
		return
	}

	var prefix string
	for _, p := range prefixes {
		if strings.HasPrefix(path, p) {
			prefix = p
			break
		}
	}
	leaf := strings.TrimPrefix(path, prefix)
	if strings.Contains(leaf, ".") {
		t.Errorf("%s: no section in settingsCoverage.sections", path)
		return
	}
	sec := c.sections[prefix]
	q := regexp.QuoteMeta(leaf)
	collectBody, _ := jsMethodBody(page.js, sec.collect)
	if !regexp.MustCompile(`\b` + q + `['"]?\s*:|\.` + q + `\s*=`).MatchString(collectBody) {
		t.Errorf("%s: %s does not send %s", path, sec.collect, leaf)
	} else if leafInfo.numeric && !sendsNumber(page, collectBody, leaf, ref) {
		t.Errorf("%s: %s sends %s without parseInt or a number input", path, sec.collect, leaf)
	}
	if sec.populate != nil && !populates(page.js, sec.populate, leaf) {
		t.Errorf("%s: %v never fill %s", path, sec.populate, leaf)
	}
}

func populates(js string, methods []string, leaf string) bool {
	word := regexp.MustCompile(`\b` + regexp.QuoteMeta(leaf) + `\b`)
	for _, m := range methods {
		if body, _ := jsMethodBody(js, m); word.MatchString(body) {
			return true
		}
	}
	return false
}

func reportUnused[V any](t *testing.T, kind string, m map[string]V, used map[string]bool) {
	t.Helper()
	for k := range m {
		if !used[k] {
			t.Errorf("%s: %s matches no config path", k, kind)
		}
	}
}

func TestSettingsPageCoversConfig(t *testing.T) {
	t.Parallel()
	var paths []configLeaf
	if err := configPaths(reflect.TypeFor[config.Config](), "", &paths); err != nil {
		t.Fatal(err)
	}
	page := readSettingsPage(t)
	cov := newSettingsCoverage()
	prefixes := cov.sortedPrefixes(t, page.js)

	usedExclusions, usedAliases := map[string]bool{}, map[string]bool{}
	for _, leaf := range paths {
		path := leaf.path
		if k, ok := matchKey(cov.exclusions, path); ok {
			usedExclusions[k] = true
			if page.mentions(cov.formRef(path)) {
				t.Errorf("%s: excluded (%s) but the page has a control for it", path, cov.exclusions[k])
			}
			continue
		}
		if k, ok := matchKey(cov.aliases, path); ok {
			usedAliases[k] = true
			if want := cov.aliases[k](path); !page.shows(want) {
				t.Errorf("%s: page lacks %s", path, want)
			}
			continue
		}
		cov.checkControl(t, page, prefixes, leaf)
	}
	reportUnused(t, "exclusion", cov.exclusions, usedExclusions)
	reportUnused(t, "alias", cov.aliases, usedAliases)
}
