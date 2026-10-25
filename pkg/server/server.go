package server

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/sessions"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/server/qbit"
	"github.com/sirrobot01/decypharr/pkg/server/sabnzbd"
	"github.com/sirrobot01/decypharr/pkg/server/webdav"
	"github.com/sirrobot01/decypharr/pkg/stats"
)

//go:embed templates/*
var content embed.FS

//go:embed assets/build/*
var assetsEmbed embed.FS

//go:embed assets/images/*
var imagesEmbed embed.FS

// Server is the HTTP front end: web UI, JSON API and the compat APIs.
const (
	sessionMaxAge = 7 * 24 * time.Hour
	// readHeaderTimeout bounds slow-header (slowloris) clients; bodies and
	// long streams are unaffected.
	readHeaderTimeout = 30 * time.Second
	shutdownGrace     = 10 * time.Second
)

type Server struct {
	config      *config.Store
	logsDir     string
	router      *chi.Mux
	logger      zerolog.Logger
	manager     *manager.Manager
	stats       *stats.Collector
	cookie      *sessions.CookieStore
	templates   *template.Template
	assets      assetManifest
	urlBase     string
	restartFunc func()
	// shutdownGrace is how long in-flight requests get to finish before open streams are closed.
	shutdownGrace time.Duration
}

// New builds the HTTP front end for one service generation. Settings that
// apply without a restart are read live from store.
func New(mgr *manager.Manager, store *config.Store, logs *logger.Factory) *Server {
	l := logs.New("http")
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.StripSlashes)
	r.Use(middleware.RedirectSlashes)

	cfg := store.Get()

	templates := template.Must(template.ParseFS(
		content,
		"templates/layout.html",
		"templates/setup_layout.html",
		"templates/index.html",
		"templates/download.html",
		"templates/repair.html",
		"templates/reacquire.html",
		"templates/repair_tabs.html",
		"templates/stats.html",
		"templates/config.html",
		"templates/browse.html",
		"templates/login.html",
		"templates/register.html",
		"templates/setup.html",
	))
	cookieStore := sessions.NewCookieStore([]byte(cfg.SecretKey()))
	cookieStore.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   int(sessionMaxAge.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}

	statsCollector := stats.New(mgr, store, logs.New("stats"))

	s := &Server{
		config:    store,
		logsDir:   logger.Dir(cfg.Dir()),
		logger:    l,
		manager:   mgr,
		stats:     statsCollector,
		cookie:    cookieStore,
		templates: templates,
		assets:    loadAssetManifest(),
		urlBase:   cfg.URLBase,

		shutdownGrace: shutdownGrace,
	}

	qb := qbit.New(mgr, store, logs.New("qbit"))
	sb := sabnzbd.New(mgr, store, logs.New("sabnzbd"))
	wd := webdav.NewHandler(mgr, store, logs.New("webdav"))

	routes := make(map[string]http.Handler)
	routes["/api/v2"] = qb.Routes()

	if !wd.IsDisabled() {
		routes["/webdav"] = wd.Routes()
	}
	// Serves the URLs written into .strm files; independent of DisableWebDav.
	routes["/stream"] = wd.StreamRoutes()
	routes["/sabnzbd"] = sb.Routes()

	// Trim trailing slash so chi registers the URLBase root path itself
	routePath := cfg.URLBase
	if routePath != "/" {
		routePath = strings.TrimSuffix(routePath, "/")
	}
	r.Route(routePath, func(r chi.Router) {
		// Mount web routes
		r.Mount("/", s.WebRoutes())

		for path, handler := range routes {
			r.Mount(path, handler)
		}

		r.Group(func(r chi.Router) {
			r.Use(s.authMiddleware)

			// logs
			r.Get("/logs", s.getLogs) // deprecated, use /debug/logs

			r.Route("/debug", func(r chi.Router) {
				r.Get("/stats", s.stats.Handler())
				r.Post("/speedtest", s.handleSpeedTest)
				r.Get("/logs", s.getLogs)
				r.Get("/logs/rclone", s.getRcloneLogs)
				r.Get("/ingests", s.handleIngests)
				r.Get("/ingests/{debrid}", s.handleIngestsByDebrid)
			})

			// Webhooks
			r.Post("/webhooks/tautulli", s.handleTautulli)
			r.Post("/webhooks/arr/{name}", s.handleArrWebhook)
		})
	})
	s.router = r
	return s
}

func (s *Server) SetRestartFunc(restartFunc func()) {
	s.restartFunc = restartFunc
}

func (s *Server) Restart() {
	if s.restartFunc != nil {
		s.restartFunc()
	} else {
		s.logger.Warn().Msg("Restart function not set")
	}
}

// Start serves HTTP until ctx is done. It returns an error when the
// listener cannot be opened or the server fails, so the process does not
// keep running without its web UI and APIs.
func (s *Server) Start(ctx context.Context) error {
	cfg := s.config.Get()

	addr := net.JoinHostPort(cfg.BindAddress, cfg.Port)
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("http server: %w", err)
	}

	// Start background stats collector
	s.stats.Start(ctx)

	s.logger.Info().Msgf("Starting server on %s%s", listener.Addr(), cfg.URLBase)
	requests, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()
	var handlers handlerTracker
	srv := &http.Server{
		Handler:           handlers.wrap(s.router),
		BaseContext:       func(net.Listener) context.Context { return requests },
		ReadHeaderTimeout: readHeaderTimeout,
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(listener)
	}()

	select {
	case err = <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		s.logger.Info().Msg("Shutting down gracefully...")
		graceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownGrace)
		defer cancel()
		if err = srv.Shutdown(graceCtx); !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		s.logger.Warn().Dur("grace", s.shutdownGrace).
			Msg("Closing requests still open after the shutdown grace period")
		cancelRequests()
		err = srv.Close()
		handlers.wait()
		return err
	}
}

// handlerTracker lets Start wait out handlers that outlive a forced close, so nothing they use is stopped under them.
type handlerTracker struct {
	mu       sync.Mutex
	stopping bool
	running  sync.WaitGroup
}

func (h *handlerTracker) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		if h.stopping {
			h.mu.Unlock()
			http.Error(w, "server is stopping", http.StatusServiceUnavailable)
			return
		}
		h.running.Add(1)
		h.mu.Unlock()
		defer h.running.Done()
		next.ServeHTTP(w, r)
	})
}

func (h *handlerTracker) wait() {
	h.mu.Lock()
	h.stopping = true
	h.mu.Unlock()
	h.running.Wait()
}

func (s *Server) getLogs(w http.ResponseWriter, _ *http.Request) {
	logFile := filepath.Join(s.logsDir, logger.FileName)

	// Open and read the file
	file, err := os.Open(logFile)
	if err != nil {
		http.Error(w, "Error reading log file", http.StatusInternalServerError)
		return
	}
	defer func(file *os.File) {
		closeErr := file.Close()
		if closeErr != nil {
			s.logger.Error().Err(closeErr).Msg("Error closing log file")
		}
	}(file)

	// Set headers
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "inline; filename=application.log")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	// Stream the file
	if _, copyErr := io.Copy(w, file); copyErr != nil {
		http.Error(w, "Error streaming log file", http.StatusInternalServerError)
		return
	}
}

func (s *Server) getRcloneLogs(w http.ResponseWriter, _ *http.Request) {
	// Rclone logs resides in the same directory as the application logs
	logFile := filepath.Join(s.logsDir, "rclone.log")
	// Open and read the file
	file, err := os.Open(logFile)
	if err != nil {
		http.Error(w, "Error reading log file", http.StatusInternalServerError)
		return
	}
	defer func(file *os.File) {
		closeErr := file.Close()
		if closeErr != nil {
			return
		}
	}(file)

	// Set headers
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "inline; filename=application.log")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	// Stream the file
	if _, copyErr := io.Copy(w, file); copyErr != nil {
		http.Error(w, fmt.Sprintf("error stremaing file %s", copyErr), http.StatusInternalServerError)
		return
	}
}
