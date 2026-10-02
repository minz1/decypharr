package sabnzbd

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// maxRequestBody caps request bodies. Forms are parsed before authentication
// (the credentials travel in them), so without a cap any client could stream
// unbounded multipart data to memory and temp files. Generous for large NZBs.
const maxRequestBody = 256 << 20

// Routes returns the SABnzbd-compatible API router.
func (s *SABnzbd) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(limitBody)
	r.Use(s.categoryContext)
	r.Use(s.authContext)

	// SABnzbd API endpoints - all under /api with mode parameter
	r.Route("/api", func(r chi.Router) {
		r.Use(s.modeContext)

		// Queue operations
		r.Get("/", s.handleAPI)
		r.Post("/", s.handleAPI)
	})

	return r
}

func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		next.ServeHTTP(w, r)
	})
}
