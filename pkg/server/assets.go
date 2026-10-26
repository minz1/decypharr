package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

type assetManifest struct {
	version string
	etags   map[string]string
}

func loadAssetManifest(static fs.FS) assetManifest {
	all := sha256.New()
	etags := map[string]string{}
	for _, tree := range []struct {
		prefix string
		root   fs.FS
	}{{"/assets", static}, {"/images", embeddedImages()}} {
		root := tree.root
		err := fs.WalkDir(root, ".", func(name string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := fs.ReadFile(root, name)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			all.Write([]byte(tree.prefix + "/" + name))
			all.Write(sum[:])
			etags[tree.prefix+"/"+name] = `"` + hex.EncodeToString(sum[:8]) + `"`
			return nil
		})
		if err != nil {
			panic(err)
		}
	}
	return assetManifest{version: hex.EncodeToString(all.Sum(nil))[:12], etags: etags}
}

func (m assetManifest) handler(prefix string, files fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A partial body would be gzipped under a plain Content-Range; assets are small, serve them whole.
		r.Header.Del("Range")
		name := prefix + path.Clean("/"+r.URL.Path)
		if strings.HasPrefix(name, "/assets/vite/.vite/") {
			http.NotFound(w, r)
			return
		}
		if etag, ok := m.etags[name]; ok {
			w.Header().Set("ETag", etag)
		}
		// Vite output names carry their content hash, so they never change under one URL.
		if r.URL.Query().Get("v") == m.version || strings.HasPrefix(name, "/assets/vite/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})
}
