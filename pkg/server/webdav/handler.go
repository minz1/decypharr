package webdav

import (
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/manager"
)

const (
	PROPFIND = "PROPFIND"
)

type Handler struct {
	config  *config.Store
	logger  *logger.RateLimitedLogger
	manager *manager.Manager

	// copyBufs holds the streamCopyBufSize buffers StreamResponse pipes
	// sessions through; every session.Read costs a lock pass and watchdog
	// arming, so copy granularity multiplies all of it. Empty means
	// allocate.
	copyBufs sync.Pool
}

// NewHandler builds the WebDAV and stream handlers. Auth settings are read
// live from cfg.
func NewHandler(mgr *manager.Manager, cfg *config.Store, log zerolog.Logger) *Handler {
	h := &Handler{
		config:  cfg,
		logger:  logger.NewRateLimitedLogger(logger.WithLogger(log)),
		manager: mgr,
	}
	return h
}

func (h *Handler) readinessMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-h.manager.IsReady():
			// WebDAV is ready, proceed
			next.ServeHTTP(w, r)
		default:
			// WebDAV is still initializing
			w.Header().Set("Retry-After", "5")
			http.Error(w, "WebDAV service is initializing, please try again shortly", http.StatusServiceUnavailable)
		}
	})
}

// Routes returns the WebDAV router.
func (h *Handler) Routes() chi.Router {
	// chi rejects unknown methods; registration is idempotent.
	for _, method := range []string{"PROPFIND", "PROPPATCH", "MKCOL", "COPY", "MOVE", "LOCK", "UNLOCK"} {
		chi.RegisterMethod(method)
	}
	r := chi.NewRouter()
	r.Use(h.readinessMiddleware)
	r.Use(h.commonMiddleware)
	r.Use(middleware.AllowContentEncoding("gzip"))
	// Always install the auth middleware; whether it actually enforces auth is
	// decided live per-request from config, so toggling UseAuth/EnableWebdavAuth
	// takes effect without rebuilding the router (no restart).
	r.Use(h.authMiddleware)

	r.HandleFunc("/", h.handleRoot)
	r.HandleFunc("/{group}", h.handleGroup)
	r.HandleFunc("/{group}/{torrent}", h.handleTorrentFolder)
	r.HandleFunc("/{group}/{torrent}/{file}", h.handleTorrentFile)
	r.HandleFunc("/stream/{group}/{torrent}/{file}", h.handleTorrentFile)
	return r
}

func (h *Handler) IsDisabled() bool {
	cfg := h.config.Get()
	return cfg.DisableWebDav
}

func (h *Handler) handler(
	current *manager.FileInfo,
	children []manager.FileInfo,
	w http.ResponseWriter,
	r *http.Request,
) {
	if current == nil && r.Method != http.MethodOptions {
		// Unknown torrent folder: HEAD used to nil-deref and PROPFIND
		// answered 207 with an empty listing.
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodHead:
		h.handleHead(current, w)
	case http.MethodGet:
		h.handleGet(current, w, r)
	case http.MethodDelete:
		h.handleDelete(current, w)
	case PROPFIND:
		h.handlePropfind(current, children, w, r)
	case http.MethodOptions:
		h.handleOptions(w)
	case "COPY", "MOVE":
		// manager.CopyEntry has never been implemented; answer honestly
		// instead of a 500.
		http.Error(w, "Not Implemented", http.StatusNotImplemented)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
}

func (h *Handler) handleRoot(w http.ResponseWriter, r *http.Request) {
	current := h.manager.RootInfo()
	children := h.manager.GetEntries()
	h.handler(current, children, w, r)
}

func (h *Handler) handleGroup(w http.ResponseWriter, r *http.Request) {
	group := utils.PathUnescape(chi.URLParam(r, "group"))
	currentInfo, rawEntries := h.manager.GetEntryChildren(group)
	if currentInfo == nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	h.handler(currentInfo, rawEntries, w, r)
}

func (h *Handler) handleTorrentFolder(w http.ResponseWriter, r *http.Request) {
	torrent := utils.PathUnescape(chi.URLParam(r, "torrent"))

	currentInfo, children := h.manager.GetTorrentChildren(torrent)
	h.handler(currentInfo, children, w, r)
}

func (h *Handler) handleTorrentFile(w http.ResponseWriter, r *http.Request) {
	torrent := utils.PathUnescape(chi.URLParam(r, "torrent"))
	file := utils.PathUnescape(chi.URLParam(r, "file"))
	currentInfo, err := h.manager.GetTorrentFile(torrent, file)
	if err != nil || currentInfo == nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	h.handler(currentInfo, nil, w, r)
}

func (h *Handler) commonMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("DAV", "1, 2")
		w.Header().
			Set("Allow", "OPTIONS, PROPFIND GET, HEAD, POST, PUT, DELETE, MKCOL, PROPPATCH, COPY, MOVE, LOCK, UNLOCK")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().
			Set("Access-Control-Allow-Methods", "OPTIONS, GET, PROPFIND, HEAD, POST, PUT, DELETE, MKCOL, PROPPATCH, COPY, MOVE, LOCK, UNLOCK")
		w.Header().Set("Access-Control-Allow-Headers", "Depth, Content-Type, Authorization")

		next.ServeHTTP(w, r)
	})
}

func (h *Handler) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the auth toggles live so changes apply without a restart.
		cfg := h.config.Get()
		if !cfg.UseAuth || !cfg.EnableWebdavAuth {
			next.ServeHTTP(w, r)
			return
		}

		username, password, ok := r.BasicAuth()
		if !ok || !h.config.Get().VerifyAuth(username, password) {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
