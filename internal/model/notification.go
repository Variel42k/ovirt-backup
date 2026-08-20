package model

import "time"

// NotificationSettings controls external delivery. Channel credentials stay
// in YAML/environment; runtime policy itself is safe to keep in PostgreSQL.
type NotificationSettings struct {
	Enabled              bool     `json:"enabled"`
	MinSeverity          Severity `json:"min_severity"`
	DefaultRepeatMinutes int      `json:"default_repeat_minutes"`
	NotifyOnResolved     bool     `json:"notify_on_resolved"`
	AckStopsRepeats      bool     `json:"ack_stops_repeats"`
	MaxRepeats           int      `json:"max_repeats"`
	ConfiguredChannels   []string `json:"configured_channels,omitempty"`
	Source               string   `json:"source"`
}

// NotificationSettingsOverride is the nullable database layer over startup
// configuration. Nil means that the corresponding YAML/environment value (or
// built-in default for the new fields) is still in force.
type NotificationSettingsOverride struct {
	Enabled              *bool
	MinSeverity          *Severity
	DefaultRepeatMinutes *int
	NotifyOnResolved     *bool
	AckStopsRepeats      *bool
	MaxRepeats           *int
}

// NotificationPolicy overrides the global policy for one alert kind.
type NotificationPolicy struct {
	Kind           string   `json:"kind"`
	Enabled        bool     `json:"enabled"`
	RepeatMinutes  int      `json:"repeat_minutes"`
	NotifyResolved bool     `json:"notify_resolved"`
	StopOnAck      bool     `json:"stop_on_ack"`
	MaxRepeats     int      `json:"max_repeats"`
	Channels       []string `json:"channels,omitempty"`
}

type NotificationEvent string

const (
	NotificationOpened   NotificationEvent = "opened"
	NotificationReminder NotificationEvent = "reminder"
	NotificationResolved NotificationEvent = "resolved"
)

// NotificationDelivery is one durable attempt for one channel. A separate
// row per channel means retrying SMTP never duplicates a successful webhook.
type NotificationDelivery struct {
	ID          string            `json:"id"`
	AlertID     string            `json:"alert_id"`
	Event       NotificationEvent `json:"event"`
	Sequence    int               `json:"sequence"`
	Channel     string            `json:"channel"`
	Status      string            `json:"status"`
	Attempts    int               `json:"attempts"`
	MaxAttempts int               `json:"max_attempts"`
	ScheduledAt time.Time         `json:"scheduled_at"`
	LastError   string            `json:"last_error,omitempty"`
	Payload     []byte            `json:"-"`
	CreatedAt   time.Time         `json:"created_at"`
	SentAt      *time.Time        `json:"sent_at,omitempty"`
}
