package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/events"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

const (
	managerPollInterval = 10 * time.Second
	managerLease        = 45 * time.Second
)

// Manager turns due alerts into a durable per-channel outbox and drains it.
// It may run in every replica: database leases make scheduling and delivery
// safe without coupling notification availability to scheduler leadership.
type Manager struct {
	store  *store.Store
	base   config.NotificationsConfig
	sender *Notifier
	bus    *events.Bus
	log    zerolog.Logger
	worker string
	wake   chan struct{}
}

func NewManager(st *store.Store, base config.NotificationsConfig, sender *Notifier,
	bus *events.Bus, log zerolog.Logger) *Manager {
	return &Manager{store: st, base: base, sender: sender, bus: bus, log: log,
		worker: "notify-" + uuid.NewString(), wake: make(chan struct{}, 1)}
}

func (m *Manager) Wake() {
	if m == nil {
		return
	}
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) Channels() []string {
	if m == nil || m.sender == nil {
		return nil
	}
	return m.sender.ChannelNames()
}

func (m *Manager) EffectiveSettings(ctx context.Context) (model.NotificationSettings, error) {
	minSeverity := model.Severity(strings.TrimSpace(m.base.MinSeverity))
	if minSeverity == "" {
		minSeverity = model.SeverityCritical
	}
	out := model.NotificationSettings{
		Enabled: m.base.Enabled, MinSeverity: minSeverity,
		DefaultRepeatMinutes: 0, NotifyOnResolved: false,
		AckStopsRepeats: true, MaxRepeats: 0,
		ConfiguredChannels: m.Channels(), Source: "config",
	}
	override, err := m.store.NotificationSettingsOverride(ctx)
	if err != nil {
		return out, err
	}
	if override.Enabled != nil {
		out.Enabled, out.Source = *override.Enabled, "database"
	}
	if override.MinSeverity != nil {
		out.MinSeverity, out.Source = *override.MinSeverity, "database"
	}
	if override.DefaultRepeatMinutes != nil {
		out.DefaultRepeatMinutes, out.Source = *override.DefaultRepeatMinutes, "database"
	}
	if override.NotifyOnResolved != nil {
		out.NotifyOnResolved, out.Source = *override.NotifyOnResolved, "database"
	}
	if override.AckStopsRepeats != nil {
		out.AckStopsRepeats, out.Source = *override.AckStopsRepeats, "database"
	}
	if override.MaxRepeats != nil {
		out.MaxRepeats, out.Source = *override.MaxRepeats, "database"
	}
	return out, nil
}

func (m *Manager) Run(ctx context.Context) {
	if m == nil {
		return
	}
	ticker := time.NewTicker(managerPollInterval)
	defer ticker.Stop()
	for {
		m.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-m.wake:
		}
	}
}

func (m *Manager) drain(ctx context.Context) {
	settings, err := m.EffectiveSettings(ctx)
	if err != nil {
		m.log.Warn().Err(err).Msg("не удалось прочитать настройки уведомлений")
		return
	}
	policies, err := m.store.ListNotificationPolicies(ctx)
	if err != nil {
		m.log.Warn().Err(err).Msg("не удалось прочитать политики уведомлений")
		return
	}
	byKind := make(map[string]model.NotificationPolicy, len(policies))
	for _, p := range policies {
		byKind[p.Kind] = p
	}

	// Bound each pass so a large retry backlog never starves alert scheduling.
	for i := 0; i < 100; i++ {
		a, ok, err := m.store.ClaimNotificationAlert(ctx, m.worker, managerLease)
		if err != nil {
			m.log.Warn().Err(err).Msg("не удалось арендовать уведомление")
			break
		}
		if !ok {
			break
		}
		m.schedule(ctx, a, settings, byKind[a.Kind])
	}
	for i := 0; i < 200; i++ {
		d, ok, err := m.store.ClaimNotificationDelivery(ctx, m.worker, managerLease)
		if err != nil {
			m.log.Warn().Err(err).Msg("не удалось арендовать доставку уведомления")
			break
		}
		if !ok {
			break
		}
		m.deliver(ctx, d)
	}
}

func (m *Manager) schedule(ctx context.Context, a *model.Alert, settings model.NotificationSettings,
	policy model.NotificationPolicy) {
	effective := policy
	if effective.Kind == "" {
		effective = model.NotificationPolicy{Kind: a.Kind, Enabled: true,
			RepeatMinutes:  settings.DefaultRepeatMinutes,
			NotifyResolved: settings.NotifyOnResolved,
			StopOnAck:      settings.AckStopsRepeats, MaxRepeats: settings.MaxRepeats}
	}
	channels := effective.Channels
	if len(channels) == 0 {
		channels = settings.ConfiguredChannels
	}
	channels = configuredOnly(channels, settings.ConfiguredChannels)
	resolved := a.State == model.AlertResolved
	allowed := settings.Enabled && effective.Enabled && severityPasses(a.Severity, settings.MinSeverity) && len(channels) > 0
	if resolved && !effective.NotifyResolved {
		allowed = false
	}
	if a.State == model.AlertAcked && effective.StopOnAck {
		allowed = false
	}
	if !resolved && effective.MaxRepeats > 0 && a.NotificationCount >= effective.MaxRepeats+1 {
		allowed = false
	}
	if !allowed {
		if err := m.store.SkipClaimedNotification(ctx, m.worker, a.ID, resolved); err != nil {
			m.log.Warn().Err(err).Str("alert", a.ID).Msg("не удалось снять аренду уведомления")
		}
		return
	}

	event := model.NotificationOpened
	sequence := a.NotificationCount + 1
	if resolved {
		event = model.NotificationResolved
		sequence = a.NotificationCount
	} else if a.NotificationCount > 0 {
		event = model.NotificationReminder
	}
	message := Message{Severity: a.Severity, Kind: a.Kind, Object: a.ObjectName,
		Text: a.Message, Details: a.Details, At: m.sender.now(), AlertID: a.ID,
		Event: event, Sequence: sequence}
	if resolved {
		message.Text = "Проблема устранена: " + a.Message
	}
	payload, err := json.Marshal(message)
	if err != nil {
		_ = m.store.SkipClaimedNotification(ctx, m.worker, a.ID, resolved)
		return
	}
	var next *time.Time
	if !resolved && effective.RepeatMinutes > 0 {
		value := time.Now().UTC().Add(time.Duration(effective.RepeatMinutes) * time.Minute)
		next = &value
	}
	if err := m.store.ScheduleNotification(ctx, m.worker, a, event, channels, payload, next); err != nil {
		m.log.Warn().Err(err).Str("alert", a.ID).Msg("не удалось поставить уведомление в outbox")
		return
	}
	if m.bus != nil {
		// No payload on purpose: the layout refreshes the alert counter, but
		// does not show a second critical toast for an outbox state update.
		m.bus.Publish(events.Event{Kind: events.KindAlert, ServerID: a.ServerID,
			ObjectID: a.ID, Message: "состояние внешней доставки обновлено"})
	}
	m.Wake()
}

func (m *Manager) deliver(ctx context.Context, d *model.NotificationDelivery) {
	var message Message
	err := json.Unmarshal(d.Payload, &message)
	if err == nil {
		err = m.sender.SendChannel(ctx, d.Channel, message)
	}
	backoff := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}
	idx := d.Attempts
	if idx >= len(backoff) {
		idx = len(backoff) - 1
	}
	retryAt := time.Now().UTC().Add(backoff[idx])
	if completeErr := m.store.CompleteNotificationDelivery(ctx, d.ID, m.worker, err, retryAt); completeErr != nil {
		m.log.Warn().Err(completeErr).Str("delivery", d.ID).Msg("не удалось обновить доставку уведомления")
		return
	}
	if err != nil {
		m.log.Warn().Err(err).Str("канал", d.Channel).Int("попытка", d.Attempts+1).
			Msg("уведомление не доставлено; повтор сохранён в PostgreSQL")
		return
	}
	m.log.Debug().Str("канал", d.Channel).Msg("уведомление доставлено")
}

func severityPasses(value, minimum model.Severity) bool {
	rank := map[model.Severity]int{model.SeverityInfo: 0, model.SeverityWarning: 1, model.SeverityCritical: 2}
	return rank[value] >= rank[minimum]
}

func configuredOnly(requested, configured []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range requested {
		if slices.Contains(configured, name) && !seen[name] {
			out = append(out, name)
			seen[name] = true
		}
	}
	return out
}

func ValidateSettings(value model.NotificationSettings) error {
	if value.MinSeverity != model.SeverityInfo && value.MinSeverity != model.SeverityWarning && value.MinSeverity != model.SeverityCritical {
		return fmt.Errorf("неизвестный уровень %q", value.MinSeverity)
	}
	if value.DefaultRepeatMinutes < 0 || value.MaxRepeats < 0 {
		return fmt.Errorf("частота и число повторов не могут быть отрицательными")
	}
	if value.DefaultRepeatMinutes > 525600 {
		return fmt.Errorf("интервал повторов не может превышать один год")
	}
	if value.MaxRepeats > 10000 {
		return fmt.Errorf("число повторов не может превышать 10000")
	}
	return nil
}

func ValidatePolicies(policies []model.NotificationPolicy, configured []string) error {
	seen := map[string]bool{}
	for _, p := range policies {
		if strings.TrimSpace(p.Kind) == "" {
			return fmt.Errorf("тип уведомления не задан")
		}
		if seen[p.Kind] {
			return fmt.Errorf("политика %q задана дважды", p.Kind)
		}
		seen[p.Kind] = true
		if p.RepeatMinutes < 0 || p.MaxRepeats < 0 {
			return fmt.Errorf("политика %q содержит отрицательное значение", p.Kind)
		}
		if p.RepeatMinutes > 525600 || p.MaxRepeats > 10000 {
			return fmt.Errorf("политика %q превышает допустимый интервал или число повторов", p.Kind)
		}
		for _, channel := range p.Channels {
			if !slices.Contains(configured, channel) {
				return fmt.Errorf("канал %q не настроен", channel)
			}
		}
	}
	return nil
}
