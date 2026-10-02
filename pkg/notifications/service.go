package notifications

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
)

// sendTimeout bounds one notifier delivery.
const sendTimeout = 30 * time.Second

// Service manages and dispatches notifications to all configured notifiers.
type Service struct {
	config    *config.Notifications
	notifiers []Notifier
	logger    zerolog.Logger
}

// New creates a new notification service based on the provided configuration.
func New(cfg *config.Notifications, logger zerolog.Logger) *Service {
	s := &Service{
		config:    cfg,
		logger:    logger.With().Str("component", "notifications").Logger(),
		notifiers: make([]Notifier, 0),
	}
	if !cfg.Enabled {
		return s
	}
	if cfg.WebhookURL != "" {
		s.notifiers = append(s.notifiers, NewDiscord(cfg.WebhookURL))
	}
	if cfg.CallbackURL != "" {
		s.notifiers = append(s.notifiers, NewCallback(cfg.CallbackURL))
	}
	return s
}

// Notify sends an event to all enabled notifiers asynchronously.
func (s *Service) Notify(event Event) {
	if !s.IsEventEnabled(event.Type) {
		return
	}
	for _, notifier := range s.notifiers {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
			defer cancel()
			if err := notifier.Send(ctx, event); err != nil {
				s.logger.Error().
					Err(err).
					Str("notifier", notifier.Name()).
					Str("event", string(event.Type)).
					Msg("Failed to send notification")
				return
			}
			s.logger.Trace().
				Str("notifier", notifier.Name()).
				Str("event", string(event.Type)).
				Msg("Notification sent successfully")
		}()
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
