package server

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
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
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/server/qbit"
	"github.com/sirrobot01/decypharr/pkg/server/sabnzbd"
	"github.com/sirrobot01/decypharr/pkg/server/webdav"
	"github.com/sirrobot01/decypharr/pkg/stats"
)

//go:embed templates/*
var content embed.FS

//go:embed all:assets/build
var assetsEmbed embed.FS

//go:embed assets/images/*
var imagesEmbed embed.FS

func embeddedStatic() fs.FS { return mustSub(assetsEmbed, "assets/build") }

func embeddedImages() fs.FS { return mustSub(imagesEmbed, "assets/images") }

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

func parseTemplates(funcs template.FuncMap) *template.Template {
	t := template.Must(template.New("").Funcs(funcs).ParseFS(content, "templates/*.html"))
	return template.Must(t.Parse(viteTagsTemplate))
}

// Server is the HTTP front end: web UI, JSON API and the compat APIs.
const (
	sessionMaxAge = 7 * 24 * time.Hour
	// readHeaderTimeout bounds slow-header (slowloris) clients; bodies and
	// long streams are unaffected.
	readHeaderTimeout = 30 * time.Second
	// shutdownGrace is the share of the stop budget that in-flight requests
	// get to finish before the streams still open are closed.
	shutdownGrace = 5 * time.Second
	// httpSlice is the server's share of the generation's stop budget for the whole drain, grace period and
	// handler wait together, so the manager keeps the rest of it.
	httpSlice = 8 * time.Second
	// defaultStopBudget bounds the shutdown of a server that was not given the
	// generation's stop context.
	defaultStopBudget = 25 * time.Second
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
	static      fs.FS
	vite        viteManifest
	assets      assetManifest
	urlBase     string
	restartFunc func()
	// tasks lists the running tasks of the generation for /debug/tasks.
	tasks func() []task.Info
	// stopCtx returns the context that ends at the generation's drain deadline.
	stopCtx func() context.Context
	// shutdownGrace is how long in-flight requests get to finish before open streams are closed.
	shutdownGrace time.Duration
	// drainSlice bounds the whole drain: the grace period and the wait for handlers after the forced close.
	drainSlice time.Duration
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

	cookieStore := sessions.NewCookieStore([]byte(cfg.SecretKey()))
	cookieStore.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   int(sessionMaxAge.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}

	statsCollector := stats.New(mgr, store, logs.New("stats"))

	s := &Server{
		config:  store,
		logsDir: logger.Dir(cfg.Dir()),
		logger:  l,
		manager: mgr,
		stats:   statsCollector,
		cookie:  cookieStore,
		urlBase: cfg.URLBase,

		shutdownGrace: shutdownGrace,
		drainSlice:    httpSlice,
	}
	s.loadUI(embeddedStatic())
	if s.vite == nil {
		l.Warn().Msg(errUINotBuilt.Error())
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
				r.Get("/tasks", s.handleDebugTasks)
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

// SetRestartFunc wires the server to the generation it belongs to. restart
// asks that generation to end and the next one to start; it must be cheap,
// idempotent and must not block.
func (s *Server) SetRestartFunc(restart func()) {
	s.restartFunc = restart
}

// SetTaskSnapshot wires GET /debug/tasks to the generation's task tree.
func (s *Server) SetTaskSnapshot(snapshot func() []task.Info) {
	s.tasks = snapshot
}

// errRestartUnavailable is returned by Restart when no generation is wired.
var errRestartUnavailable = errors.New("restart is not available: the server is not attached to a generation")

// Restart asks the generation this server belongs to for a restart. It never
// blocks and a second call within the same generation changes nothing.
func (s *Server) Restart() error {
	if s.restartFunc == nil {
		return errRestartUnavailable
	}
	s.restartFunc()
	return nil
}

// Start opens the listener and serves HTTP on tasks of g until g stops. It
// returns an error when the listener cannot be opened, and the group fails if
// the server dies later, so the process does not keep running without its web
// UI and APIs. g belongs to the server: stopping g drains in-flight requests
// for the shutdown grace period, then closes the streams still open and waits
// for their handlers.
func (s *Server) Start(g *task.Group) error {
	cfg := s.config.Get()
	ctx := g.Context()

	addr := net.JoinHostPort(cfg.BindAddress, cfg.Port)
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("http server: %w", err)
	}

	// Start background stats collector
	if err = g.Go("stats", func(ctx context.Context) error {
		s.stats.Run(ctx)
		return nil
	}); err != nil {
		_ = listener.Close()
		return err
	}

	s.logger.Info().Msgf("Starting server on %s%s", listener.Addr(), cfg.URLBase)
	requests, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	var handlers handlerTracker
	srv := &http.Server{
		Handler:           handlers.wrap(s.router),
		BaseContext:       func(net.Listener) context.Context { return requests },
		ReadHeaderTimeout: readHeaderTimeout,
	}

	if err = g.GoCritical("serve", func(context.Context) error {
		serveErr := srv.Serve(listener)
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("http server: %w", serveErr)
	}); err != nil {
		cancelRequests()
		_ = listener.Close()
		return err
	}
	if err = g.Go("shutdown", func(ctx context.Context) error {
		defer cancelRequests()
		<-ctx.Done()
		return s.shutdown(ctx, srv, &handlers, cancelRequests)
	}); err != nil {
		cancelRequests()
		_ = listener.Close()
		return err
	}
	return nil
}

// shutdown drains srv: in-flight requests get the grace period, then the
// streams still open are closed and their handlers waited for. Both waits are
// bounded by the server's slice of the stop context (drainSlice), which the stop
// context bounds in turn, so the manager keeps the rest of the budget.
func (s *Server) shutdown(
	ctx context.Context,
	srv *http.Server,
	handlers *handlerTracker,
	cancelRequests context.CancelFunc,
) error {
	s.logger.Info().Msg("Shutting down gracefully...")
	stop, release := s.stopContext(ctx)
	defer release()
	drain, cancelDrain := context.WithTimeout(stop, s.drainSlice)
	defer cancelDrain()
	graceCtx, cancel := context.WithTimeout(drain, s.shutdownGrace)
	defer cancel()
	err := srv.Shutdown(graceCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	s.logger.Warn().Dur("grace", s.shutdownGrace).
		Msg("Closing requests still open after the shutdown grace period")
	cancelRequests()
	closeErr := srv.Close()
	if waitErr := handlers.wait(drain); waitErr != nil {
		return errors.Join(closeErr, fmt.Errorf("handlers still running at the end of the drain: %w", waitErr))
	}
	return closeErr
}

// stopContext is the context that bounds the shutdown: the one the generation
// wired in, or, for a server without one, a detached context with the default
// budget. The returned func releases it.
func (s *Server) stopContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.stopCtx != nil {
		return s.stopCtx(), func() {}
	}
	return task.Detached(ctx, defaultStopBudget)
}

// SetStopContext wires the server's shutdown to the generation's stop
// context, which ends at the generation's drain deadline.
func (s *Server) SetStopContext(stop func() context.Context) {
	s.stopCtx = stop
}

// handlerTracker lets Start wait out handlers that outlive a forced close, so
// nothing they use is stopped under them.
type handlerTracker struct {
	mu       sync.Mutex
	stopping bool
	running  int
	idle     chan struct{} // closed when running drops to zero while a wait is pending
}

func (h *handlerTracker) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		if h.stopping {
			h.mu.Unlock()
			http.Error(w, "server is stopping", http.StatusServiceUnavailable)
			return
		}
		h.running++
		h.mu.Unlock()
		defer h.done()
		next.ServeHTTP(w, r)
	})
}

func (h *handlerTracker) done() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.running--
	if h.running == 0 && h.idle != nil {
		close(h.idle)
		h.idle = nil
	}
}

// wait refuses new requests and waits until the running ones return, or until
// ctx ends, in which case it reports ctx's error.
func (h *handlerTracker) wait(ctx context.Context) error {
	h.mu.Lock()
	h.stopping = true
	if h.running == 0 {
		h.mu.Unlock()
		return nil
	}
	idle := make(chan struct{})
	h.idle = idle
	h.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
