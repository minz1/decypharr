package server

import (
	"encoding"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
)

// settingsExclusions lists the config leaves the settings page leaves alone, with the reason; keys ending in "." match
// a whole subtree.
func settingsExclusions() map[string]string {
	return map[string]string{
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

func matchKey[V any](m map[string]V, path string) (string, bool) {
	for k := range m {
		if k == path || (strings.HasSuffix(k, ".") && strings.HasPrefix(path, k)) {
			return k, true
		}
	}
	return "", false
}

// sampleChoices holds a valid non-default value for each leaf the page shows as a choice, a number or checked.
func sampleChoices() map[string]any {
	return map[string]any{
		"port":                                    "18282",
		"log_level":                               "trace",
		"folder_naming":                           "infohash",
		"default_download_action":                 "none",
		"strm.delivery_mode":                      "redirect",
		"usenet.body_pipeline_depth":              3,
		"mount.type":                              "rclone",
		"mount.rclone.log_level":                  "ERROR",
		"mount.rclone.vfs_cache_mode":             "full",
		"mount.rclone.async_read":                 false,
		"repair.source":                           "managed",
		"repair.strategy":                         "per_file",
		"prune.mode":                              "delete",
		"hearsay.advice_mode":                     "active",
		"debrids[].provider":                      "torbox",
		"notifications.events":                    []string{"download_failed", "repair_cancelled"},
		"virtual_folders[].match":                 "any",
		"virtual_folders[].conditions[].field":    "file_name",
		"virtual_folders[].conditions[].operator": "not_contains",
		"strm.download_sidecars":                  false,
		"hearsay.participate":                     false,
		"hearsay.publish":                         false,
	}
}

// sampleResponseExclusions are configResponse keys outside Config; the auth endpoints own them.
func sampleResponseExclusions() map[string]string {
	return map[string]string{
		"api_token":       "managed elsewhere",
		"auth_username":   "managed elsewhere",
		"auth_token_only": "managed elsewhere",
	}
}

func jsonName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	return name
}

func sampleField(v reflect.Value, name string) (reflect.Value, error) {
	for f := range v.Type().Fields() {
		if jsonName(f) == name {
			return v.FieldByIndex(f.Index), nil
		}
	}
	return reflect.Value{}, fmt.Errorf("no field %s in %s", name, v.Type())
}

func allocated(v reflect.Value) reflect.Value {
	if v.Kind() != reflect.Pointer {
		return v
	}
	if v.IsNil() {
		v.Set(reflect.New(v.Type().Elem()))
	}
	return v.Elem()
}

func setSampleLeaf(v reflect.Value, value any) error {
	switch typed := value.(type) {
	case string:
		v.SetString(typed)
	case bool:
		v.SetBool(typed)
	case int:
		if v.CanUint() {
			v.SetUint(uint64(typed))
		} else {
			v.SetInt(int64(typed))
		}
	case float64:
		v.SetFloat(typed)
	case []string:
		v.Set(reflect.MakeSlice(v.Type(), len(typed), len(typed)))
		for i, item := range typed {
			v.Index(i).SetString(item)
		}
	default:
		return fmt.Errorf("unsupported sample value %T", value)
	}
	return nil
}

func samplePath(root reflect.Value, path string) (reflect.Value, error) {
	v := root
	for segment := range strings.SplitSeq(path, ".") {
		name, list := strings.CutSuffix(segment, "[]")
		field, err := sampleField(v, name)
		if err != nil {
			return reflect.Value{}, err
		}
		v = allocated(field)
		if list {
			if v.Len() == 0 {
				v.Set(reflect.MakeSlice(v.Type(), 1, 1))
			}
			v = v.Index(0)
		}
	}
	return v, nil
}

func defaultSampleValue(leaf configLeaf, kind reflect.Kind, n int) any {
	switch {
	case kind == reflect.Bool:
		return true
	case kind == reflect.Float32 || kind == reflect.Float64:
		return float64(n) + 0.5
	case leaf.numeric:
		return n
	case kind == reflect.Slice:
		return []string{leaf.path}
	default:
		return leaf.path
	}
}

// configSample gives each leaf the settings page owns a distinct non-default value, with one item per list.
func configSample(t *testing.T) config.Config {
	t.Helper()
	var leaves []configLeaf
	if err := configPaths(reflect.TypeFor[config.Config](), "", &leaves); err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	root := reflect.ValueOf(&cfg).Elem()
	choices := sampleChoices()
	const firstNumber = 1000
	for i, leaf := range leaves {
		v, err := samplePath(root, leaf.path)
		if err != nil {
			t.Fatalf("%s: %v", leaf.path, err)
		}
		if v.Kind() == reflect.Map {
			continue
		}
		value, chosen := choices[leaf.path]
		if !chosen {
			value = defaultSampleValue(leaf, v.Kind(), firstNumber+i)
		}
		if setErr := setSampleLeaf(v, value); setErr != nil {
			t.Fatalf("%s: %v", leaf.path, setErr)
		}
	}
	cfg.Arrs[0].SelectedDebrid = cfg.Debrids[0].Name
	cfg.Repair.Arrs = []string{cfg.Arrs[0].Name}
	// The page sends catalog rules by id alone; their match text comes from the catalog.
	cfg.QueueCleanup.Rules = nil
	for _, rule := range config.DefaultQueueCleanupRules() {
		cfg.QueueCleanup.Rules = append(
			cfg.QueueCleanup.Rules,
			config.QueueCleanupRule{ID: rule.ID, Action: "blacklist"},
		)
	}
	cfg.QueueCleanup.Rules = append(cfg.QueueCleanup.Rules, config.QueueCleanupRule{
		Match:  "queue_cleanup.rules[].match",
		Action: "import",
	})
	cfg.Auth = &config.Auth{Username: "admin", APIToken: "sample-token"}
	return cfg
}

func configSampleFiles(t *testing.T) map[string][]byte {
	t.Helper()
	cfg := configSample(t)
	sample, err := json.MarshalIndent(configResponse(cfg, cfg.Arrs), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	exclusions := maps.Clone(settingsExclusions())
	maps.Copy(exclusions, sampleResponseExclusions())
	excluded, err := json.MarshalIndent(exclusions, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return map[string][]byte{"config-sample.json": sample, "config-exclusions.json": excluded}
}

func sampleJSONPaths(value any, path string, out map[string]bool) {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if path != "" {
				key = path + "." + key
			}
			sampleJSONPaths(item, key, out)
		}
	case []any:
		objects := false
		for _, item := range typed {
			if object, isObject := item.(map[string]any); isObject {
				objects = true
				sampleJSONPaths(object, path+"[]", out)
			}
		}
		out[path] = !objects
	default:
		out[path] = true
	}
}

func TestConfigSampleSetsEveryLeaf(t *testing.T) {
	t.Parallel()
	var sample any
	if err := json.Unmarshal(configSampleFiles(t)["config-sample.json"], &sample); err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	sampleJSONPaths(sample, "", paths)
	var leaves []configLeaf
	if err := configPaths(reflect.TypeFor[config.Config](), "", &leaves); err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{}
	for _, leaf := range leaves {
		key, excluded := matchKey(settingsExclusions(), leaf.path)
		used[key] = excluded
		if !excluded && !paths[leaf.path] {
			t.Errorf("%s: missing from the sample", leaf.path)
		}
	}
	for key := range settingsExclusions() {
		if !used[key] {
			t.Errorf("%s: exclusion matches no config path", key)
		}
	}
}
