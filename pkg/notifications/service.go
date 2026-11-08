package notifications

import (
	"crypto/tls"
	"errors"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/internal/task/keyed"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// Service manages and dispatches notifications to all configured notifiers.
type Service struct {
	config    *config.Notifications
	notifiers []Notifier
	logger    zerolog.Logger
	runner    *keyed.Runner[string, msg, notifierState]
}

// New creates a new notification service based on the provided configuration.
// tlsConfig is the verified TLS base (the configured CA file). Deliveries run
// on g: stopping it cancels the sends in flight and drops what is queued.
func New(g *task.Group, cfg *config.Notifications, tlsConfig *tls.Config, logger zerolog.Logger) *Service {
	var notifiers []Notifier
	if cfg.Enabled {
		client := utils.NewHTTPClient(tlsConfig, 0) // each send is bounded by sendTimeout
		if cfg.WebhookURL != "" {
			notifiers = append(notifiers, NewDiscord(cfg.WebhookURL, client))
		}
		if cfg.CallbackURL != "" {
			notifiers = append(notifiers, NewCallback(cfg.CallbackURL, client))
		}
	}
	return newService(g, cfg, notifiers, logger, nopObserver{})
}

func newService(
	g *task.Group,
	cfg *config.Notifications,
	notifiers []Notifier,
	logger zerolog.Logger,
	obs observer,
) *Service {
	logger = logger.With().Str("component", "notifications").Logger()
	return &Service{
		config:    cfg,
		notifiers: notifiers,
		logger:    logger,
		runner:    newRunner(g, notifiers, logger, obs),
	}
}

// Notify hands an event to every notifier's queue and returns at once. The
// event's entry is copied first, so the caller may go on changing it.
func (s *Service) Notify(event Event) {
	if !s.IsEventEnabled(event.Type) || len(s.notifiers) == 0 {
		return
	}
	if e := event.Entry; e != nil {
		event.Entry = &storage.Entry{
			InfoHash:       e.InfoHash,
			Name:           e.Name,
			Category:       e.Category,
			ActiveProvider: e.ActiveProvider,
			ContentPath:    e.ContentPath,
		}
	}
	for _, n := range s.notifiers {
		err := s.runner.Send(n.Name(), msg{kind: msgEnqueue, ev: event})
		switch {
		case err == nil:
		case errors.Is(err, keyed.ErrStopped):
			s.logger.Debug().
				Str("notifier", n.Name()).
				Str("event", string(event.Type)).
				Msg("Notification dropped: stopping")
		default:
			s.logger.Warn().
				Err(err).
				Str("notifier", n.Name()).
				Str("event", string(event.Type)).
				Msg("Notification dropped")
		}
	}
}

// IsEventEnabled checks if a specific event type is enabled for notifications.
func (s *Service) IsEventEnabled(eventType config.NotificationEvent) bool {
	return s.config.IsEventEnabled(eventType)
}

// IsEnabled returns whether notifications are globally enabled.
func (s *Service) IsEnabled() bool {
	return s.config.Enabled && len(s.notifiers) > 0
}
