package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"strings"
)

const viteManifestPath = "vite/.vite/manifest.json"

var errUINotBuilt = errors.New("UI assets not built; run `pnpm build`")

type viteChunk struct {
	File    string   `json:"file"`
	CSS     []string `json:"css"`
	Imports []string `json:"imports"`
}

type viteManifest map[string]viteChunk

func loadViteManifest(fsys fs.FS) (viteManifest, error) {
	data, err := fs.ReadFile(fsys, viteManifestPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errUINotBuilt
	}
	if err != nil {
		return nil, err
	}
	var m viteManifest
	if jsonErr := json.Unmarshal(data, &m); jsonErr != nil {
		return nil, fmt.Errorf("parse %s: %w", viteManifestPath, jsonErr)
	}
	return m, nil
}

// viteTagsTemplate renders a viteCSS or viteEntry result, so html/template escapes every URL.
const viteTagsTemplate = `{{define "vite"}}` +
	`{{range .CSS}}<link rel="stylesheet" href="{{.}}">{{end}}` +
	`{{with .Script}}<script type="module" src="{{.}}"></script>{{end}}` +
	`{{range .Preload}}<link rel="modulepreload" href="{{.}}">{{end}}` +
	`{{end}}`

type viteTags struct {
	CSS     []string
	Script  string
	Preload []string
}

func (m viteManifest) funcs(urlBase string) template.FuncMap {
	return template.FuncMap{
		"viteCSS":   func(entry string) (viteTags, error) { return m.cssTags(urlBase, entry) },
		"viteEntry": func(entry string) (viteTags, error) { return m.entryTags(urlBase, entry) },
	}
}

func (m viteManifest) cssTags(urlBase, entry string) (viteTags, error) {
	return m.tags(urlBase, entry, ".css")
}

func (m viteManifest) entryTags(urlBase, entry string) (viteTags, error) {
	return m.tags(urlBase, entry, ".js")
}

// tags follows Vite's backend-integration order: CSS, imported chunks' CSS, the entry, modulepreloads.
func (m viteManifest) tags(urlBase, entry, ext string) (viteTags, error) {
	c, ok := m[entry]
	if !ok {
		return viteTags{}, fmt.Errorf("vite manifest has no entry %q", entry)
	}
	if !strings.HasSuffix(c.File, ext) {
		return viteTags{}, fmt.Errorf("vite entry %q builds %s, not a %s file", entry, c.File, ext)
	}
	imported, err := m.importedChunks(c, map[string]bool{})
	if err != nil {
		return viteTags{}, err
	}
	url := func(file string) string { return urlBase + "assets/vite/" + file }
	var t viteTags
	for _, ch := range append([]viteChunk{c}, imported...) {
		for _, css := range ch.CSS {
			t.CSS = append(t.CSS, url(css))
		}
	}
	if ext == ".css" {
		t.CSS = append(t.CSS, url(c.File))
		return t, nil
	}
	t.Script = url(c.File)
	for _, ch := range imported {
		t.Preload = append(t.Preload, url(ch.File))
	}
	return t, nil
}

func (m viteManifest) importedChunks(c viteChunk, seen map[string]bool) ([]viteChunk, error) {
	var out []viteChunk
	for _, key := range c.Imports {
		if seen[key] {
			continue
		}
		seen[key] = true
		imp, ok := m[key]
		if !ok {
			return nil, fmt.Errorf("vite manifest has no chunk %q", key)
		}
		nested, err := m.importedChunks(imp, seen)
		if err != nil {
			return nil, err
		}
		out = append(append(out, nested...), imp)
	}
	return out, nil
}
