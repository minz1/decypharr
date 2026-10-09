package arr

import (
	"cmp"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/rs/zerolog"
	"golang.org/x/sync/singleflight"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/internal/utils"
)

// Service owns every configured Arr instance and every call made to one.
type Service struct {
	mu   sync.RWMutex
	arrs map[string]Arr

	config   *config.Store
	client   *request.Client
	mutation *request.Client
	logger   zerolog.Logger
	cleanups singleflight.Group
}

// readRetries is how many times the shared client retries an Arr read.
const readRetries = 5

// New builds the service from the Arr instances configured in cfg. The
// instance list and queue cleanup rules are read live from cfg.
func New(cfg *config.Store, tlsConfig *tls.Config, log zerolog.Logger) *Service {
	service := &Service{
		arrs:   make(map[string]Arr),
		config: cfg,
		logger: log,
		client: request.New(
			log,
			tlsConfig,
			request.WithTimeout(0),
			request.WithMaxRetries(readRetries),
		),
		// Mutations are not retried: a repeated blocklist or search is a second
		// user-visible action, not a second read.
		mutation: request.New(
			log,
			tlsConfig,
			request.WithTimeout(0),
			request.WithMaxRetries(0),
		),
	}
	for _, configured := range cfg.Get().Arrs {
		instance := fromConfig(configured)
		if !instance.Reachable() || utils.ValidateURL(instance.Host) != nil {
			continue
		}
		service.arrs[instance.Name] = instance
	}
	return service
}

func (s *Service) Get(name string) (Arr, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	instance, ok := s.arrs[name]
	return instance, ok
}

// MatchCredentials checks saved Arr credentials without a network request.
// An empty category matches any Arr, which lets a client log in before it
// sends its category. An empty host matches on the API key alone (a Bearer
// key). An Arr registered from client credentials is never trusted unless the
// same credentials are also saved in the config.
func (s *Service) MatchCredentials(category, host, token string) (Arr, bool) {
	if token == "" {
		return Arr{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for name, instance := range s.arrs {
		if category != "" && name != category {
			continue
		}
		if (host != "" && host != instance.Host) ||
			subtle.ConstantTimeCompare([]byte(token), []byte(instance.Token)) != 1 {
			continue
		}
		if instance.Source != SourceAuto || s.savedCredentials(name, instance.Host, token) {
			return instance, true
		}
	}
	return Arr{}, false
}

// savedCredentials reports whether the config holds name with this host and
// token. Caller must hold s.mu.
func (s *Service) savedCredentials(name, host, token string) bool {
	return slices.ContainsFunc(s.config.Get().Arrs, func(configured config.Arr) bool {
		return configured.Name == name && configured.Host == host &&
			subtle.ConstantTimeCompare([]byte(token), []byte(strings.TrimSpace(configured.Token))) == 1
	})
}

// GetOrCreate returns a placeholder for a category with no configured Arr, so
// a download can still be categorised.
func (s *Service) GetOrCreate(name string) Arr {
	if name == "" {
		name = "uncategorized"
	}
	if instance, ok := s.Get(name); ok {
		return instance
	}
	return Arr{Name: name, Type: inferType("", name), Source: SourceManual}
}

func (s *Service) All() []Arr {
	s.mu.RLock()
	defer s.mu.RUnlock()
	instances := slices.Collect(maps.Values(s.arrs))
	slices.SortFunc(instances, func(left, right Arr) int {
		return cmp.Compare(left.Name, right.Name)
	})
	return instances
}

func (s *Service) AddOrUpdate(instance Arr) {
	if !instance.Reachable() || utils.ValidateURL(instance.Host) != nil {
		return
	}
	if instance.Type == "" {
		instance.Type = inferType(instance.Host, instance.Name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.arrs[instance.Name] = instance
}

func (s *Service) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.arrs)
}

func (s *Service) SyncFromConfig(configured []config.Arr) {
	updated := make(map[string]Arr, len(configured))
	for _, c := range configured {
		updated[c.Name] = fromConfig(c)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for name, current := range s.arrs {
		instance, ok := updated[name]
		if !ok {
			updated[name] = current
			continue
		}
		// Keep the resolved host only when the configured one is unusable.
		if utils.ValidateURL(instance.Host) != nil {
			instance.Host = current.Host
		}
		instance.Token = cmp.Or(instance.Token, current.Token)
		updated[name] = instance
	}
	s.arrs = updated
}

func (s *Service) SyncToConfig() []config.Arr {
	merged := make(map[string]config.Arr)
	for _, c := range s.config.Get().Arrs {
		if c.Host == "" || c.Token == "" {
			continue
		}
		merged[c.Name] = c
	}

	for _, instance := range s.All() {
		existing, ok := merged[instance.Name]
		if !ok {
			merged[instance.Name] = instance.toConfig()
			continue
		}
		if utils.ValidateURL(instance.Host) == nil {
			existing.Host = instance.Host
		}
		existing.Token = cmp.Or(existing.Token, instance.Token)
		existing.SkipRepair = instance.SkipRepair
		existing.DownloadUncached = instance.DownloadUncached
		existing.SelectedDebrid = instance.SelectedDebrid
		merged[instance.Name] = existing
	}
	return slices.Collect(maps.Values(merged))
}

// ResolveType records the application an instance reported, so a name that
// does not say "sonarr" or "radarr" is still classified correctly.
func (s *Service) ResolveType(ctx context.Context, name string) Type {
	instance, ok := s.Get(name)
	if !ok {
		return Others
	}
	if instance.Type == Sonarr || instance.Type == Radarr {
		return instance.Type
	}
	kind, err := s.Probe(ctx, instance)
	if err != nil {
		s.logger.Debug().Err(err).Str("arr", name).Msg("Could not detect arr type")
		return instance.Type
	}
	if kind == Others {
		return instance.Type
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if current, found := s.arrs[name]; found {
		current.Type = kind
		s.arrs[name] = current
	}
	return kind
}

// CleanupQueues runs the configured queue cleanup rules against every
// instance, one run per instance at a time.
func (s *Service) CleanupQueues(ctx context.Context) {
	var wg sync.WaitGroup
	for _, instance := range s.All() {
		wg.Go(func() {
			_, err, _ := s.cleanups.Do(instance.Name, func() (any, error) {
				return nil, s.CleanupQueue(ctx, instance.Name)
			})
			if err != nil {
				s.logger.Error().Err(err).Str("arr", instance.Name).Msg("Failed to clean up arr queue")
			}
		})
	}
	wg.Wait()
}

func (s *Service) instance(name string) (Arr, error) {
	instance, ok := s.Get(name)
	if !ok || !instance.Reachable() {
		return Arr{}, fmt.Errorf("%w: %q", ErrNotConfigured, name)
	}
	return instance, nil
}

func (s *Service) instanceOfType(name string, want Type) (Arr, error) {
	instance, err := s.instance(name)
	if err != nil {
		return Arr{}, err
	}
	if instance.Type != want {
		return Arr{}, fmt.Errorf("%w: arr %q is %s, want %s", ErrUnsupportedType, name, instance.Type, want)
	}
	return instance, nil
}
