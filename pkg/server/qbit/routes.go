package qbit

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// maxRequestBody caps request bodies. categoryContext parses forms before
// authentication, so without a cap any client could stream unbounded
// multipart data to memory and temp files. Torrent files are far smaller.
const maxRequestBody = 256 << 20

// Routes returns the qBittorrent-compatible API router.
func (q *QBit) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
			next.ServeHTTP(w, r)
		})
	})
	r.Use(q.categoryContext)
	r.Group(func(r chi.Router) {
		r.Post("/auth/login", q.handleLogin)
		r.Route("/torrents", func(r chi.Router) {
			r.Use(hashesContext)
			r.Use(q.authContext)

			r.Get("/info", q.handleTorrentsInfo)
			r.Post("/info", q.handleTorrentsInfo)

			r.Post("/add", q.handleTorrentsAdd)
			r.Post("/delete", q.handleTorrentsDelete)

			r.Get("/categories", q.handleCategories)
			r.Post("/categories", q.handleCategories)

			r.Post("/createCategory", q.handleCreateCategory)
			r.Post("/setCategory", q.handleSetCategory)
			r.Post("/addTags", q.handleAddTorrentTags)
			r.Post("/removeTags", q.handleRemoveTorrentTags)
			r.Post("/createTags", q.handleCreateTags)

			r.Get("/tags", q.handleGetTags)
			r.Get("/pause", q.handleTorrentsNoop)
			r.Get("/resume", q.handleTorrentsNoop)
			r.Get("/recheck", q.handleTorrentsNoop)
			r.Get("/properties", q.handleTorrentProperties)
			r.Get("/files", q.handleTorrentFiles)

			// Create POST equivalents for pause, resume, recheck
			r.Post("/tags", q.handleGetTags)
			r.Post("/pause", q.handleTorrentsNoop)
			r.Post("/resume", q.handleTorrentsNoop)
			r.Post("/recheck", q.handleTorrentsNoop)
			r.Post("/properties", q.handleTorrentProperties)
			r.Post("/files", q.handleTorrentFiles)
		})

		r.Route("/app", func(r chi.Router) {
			r.Get("/version", q.handleVersion)
			r.Get("/webapiVersion", q.handleWebAPIVersion)
			r.With(q.authContext).Get("/preferences", q.handlePreferences)
			r.Get("/buildInfo", q.handleBuildInfo)
			r.Get("/shutdown", q.handleShutdown)
		})
	})
	return r
}
