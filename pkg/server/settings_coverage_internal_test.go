package server

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
)

type settingsSection struct {
	populate []string
	collect  string
}

var settingsSections = map[string]settingsSection{
	"":                       {[]string{"populateForm", "populateGeneralSettings", "populateDownloadSettings"}, "collectFormData"},
	"hearsay.":               {[]string{"populateHearsaySettings"}, "collectHearsayConfig"},
	"repair.":                {[]string{"populateRepairSettings"}, "collectRepairConfig"},
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
}

var settingsListNames = [][2]string{
	{"debrids[].", "debrid[${index}]."},
	{"arrs[].", "arr[${index}]."},
	{"usenet.providers[].", "usenet.providers[${index}]."},
}

// Keys ending in "." match a whole subtree; the value is the text the page must contain.
var settingsAliases = map[string]func(path string) string{
	"strm.":                  func(p string) string { return `name="strm_` + strings.TrimPrefix(p, "strm.") + `"` },
	"hearsay.disabled":       func(string) string { return `name="hearsay.enabled"` },
	"notifications.events":   func(string) string { return `name="notifications.events[]"` },
	"repair.arrs":            func(string) string { return `id="repair.arrs"` },
	"virtual_folders[].":     func(string) string { return "collectVirtualFolders(" },
	"queue_cleanup.rules[].": func(string) string { return "collectQueueCleanup(" },
}

var settingsExclusions = map[string]string{
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
	"enable_webdav_auth":        "phase 2",
	"prune.":                    "phase 3",
}

func configPaths(t reflect.Type, prefix string, out *[]string) error {
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
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
			*out = append(*out, p)
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

func matchKey(m map[string]string, path string) (string, bool) {
	for k := range m {
		if k == path || (strings.HasSuffix(k, ".") && strings.HasPrefix(path, k)) {
			return k, true
		}
	}
	return "", false
}

func TestSettingsPageCoversConfig(t *testing.T) {
	var paths []string
	if err := configPaths(reflect.TypeFor[config.Config](), "", &paths); err != nil {
		t.Fatal(err)
	}

	read := func(name string) string {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return regexp.MustCompile(`\$\{[^}]*\}`).ReplaceAllLiteralString(string(b), "${index}")
	}
	html := read("templates/config.html")
	js := read("assets/js/config.js")
	build := read("assets/build/js/config.js")

	prefixes := make([]string, 0, len(settingsSections))
	for p, s := range settingsSections {
		prefixes = append(prefixes, p)
		for _, m := range append(s.populate, s.collect) {
			if _, ok := jsMethodBody(js, m); !ok {
				t.Errorf("%s: method %s not in config.js", p, m)
			}
		}
	}
	sort.Slice(prefixes, func(i, j int) bool { return len(prefixes[i]) > len(prefixes[j]) })

	aliasKeys := make(map[string]string, len(settingsAliases))
	for k := range settingsAliases {
		aliasKeys[k] = k
	}
	usedExclusions := map[string]bool{}
	usedAliases := map[string]bool{}

	for _, path := range paths {
		formName := path
		for _, l := range settingsListNames {
			formName = strings.Replace(formName, l[0], l[1], 1)
		}
		ref := `name="` + formName + `"`
		inSrc, inBuild := strings.Contains(js, ref), strings.Contains(build, ref)
		named := strings.Contains(html, ref) || (inSrc && inBuild)

		if k, ok := matchKey(settingsExclusions, path); ok {
			usedExclusions[k] = true
			if strings.Contains(html, ref) || inSrc || inBuild {
				t.Errorf("%s: excluded (%s) but the page has a control for it", path, settingsExclusions[k])
			}
			continue
		}
		if k, ok := matchKey(aliasKeys, path); ok {
			usedAliases[k] = true
			want := settingsAliases[k](path)
			if !strings.Contains(html, want) && !(strings.Contains(js, want) && strings.Contains(build, want)) {
				t.Errorf("%s: page lacks %s", path, want)
			}
			continue
		}
		if !named {
			switch {
			case inSrc:
				t.Errorf("%s: %s in config.js but not in the build; run node scripts/minify-js.js", path, ref)
			default:
				t.Errorf("%s: no control %s", path, ref)
			}
			continue
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
			t.Errorf("%s: no section in settingsSections", path)
			continue
		}
		sec := settingsSections[prefix]
		q := regexp.QuoteMeta(leaf)
		collectBody, _ := jsMethodBody(js, sec.collect)
		if !regexp.MustCompile(`\b` + q + `['"]?\s*:|\.` + q + `\s*=`).MatchString(collectBody) {
			t.Errorf("%s: %s does not send %s", path, sec.collect, leaf)
		}
		if sec.populate == nil {
			continue
		}
		word := regexp.MustCompile(`\b` + q + `\b`)
		populated := false
		for _, m := range sec.populate {
			body, _ := jsMethodBody(js, m)
			populated = populated || word.MatchString(body)
		}
		if !populated {
			t.Errorf("%s: %v never fill %s", path, sec.populate, leaf)
		}
	}

	for k := range settingsExclusions {
		if !usedExclusions[k] {
			t.Errorf("%s: exclusion matches no config path", k)
		}
	}
	for k := range settingsAliases {
		if !usedAliases[k] {
			t.Errorf("%s: alias matches no config path", k)
		}
	}
}
