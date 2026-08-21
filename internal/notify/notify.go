// Package notify доставляет критические оповещения наружу: почтой, webhook-ом
// и в Telegram.
//
// Наружу — потому что внутрь их уже видно. Оповещение, о котором узнаёт только
// тот, кто в этот момент смотрит в интерфейс, ночью не работает, а именно
// ночью и идут бэкапы.
package notify

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Message — то, что уходит человеку.
type Message struct {
	Severity model.Severity
	Kind     string
	Object   string
	Text     string
	Details  string
	At       time.Time
	AlertID  string
	Event    model.NotificationEvent
	Sequence int
}

// Channel — способ одной попытки доставки. Ошибку возвращает durable manager:
// он сохраняет её и планирует ограниченный повтор именно этого канала.
type Channel interface {
	Name() string
	Send(ctx context.Context, m Message) error
}

const (
	// queueSize ограничивает очередь. Она заполняется только если канал
	// отвечает медленнее, чем приходят события; терять сообщения в этом случае
	// правильнее, чем задерживать опрос гипервизоров.
	queueSize = 256
	// sendTimeout — предел на одну отправку. SMTP-сервер, отвечающий минуту,
	// не должен превращать очередь в пробку.
	sendTimeout = 20 * time.Second
)

// Notifier рассылает сообщения по настроенным каналам.
type Notifier struct {
	channels    []Channel
	minSeverity model.Severity
	log         zerolog.Logger

	queue    chan Message
	wg       sync.WaitGroup
	once     sync.Once
	done     chan struct{}
	location atomic.Pointer[time.Location]

	// dropped считает потерянные из-за переполнения. Молчаливая потеря
	// оповещения — худшее, что может сделать система оповещений, поэтому она
	// хотя бы попадает в журнал.
	mu      sync.Mutex
	dropped int
}

// New собирает отправителя по настройкам. Возвращает nil, если оповещения
// выключены или не настроен ни один канал: пустой отправитель ничем не лучше
// отсутствующего, а проверять на nil вызывающему всё равно придётся.
func New(cfg config.NotificationsConfig, log zerolog.Logger) *Notifier {
	return newNotifier(cfg, log, true)
}

// NewSender builds configured channels even when runtime delivery is disabled
// in YAML. The durable manager applies the effective database override before
// sending; without this constructor the UI could enable a configured channel
// only after a process restart.
func NewSender(cfg config.NotificationsConfig, log zerolog.Logger) *Notifier {
	return newNotifier(cfg, log, false)
}

func newNotifier(cfg config.NotificationsConfig, log zerolog.Logger, requireEnabled bool) *Notifier {
	if requireEnabled && !cfg.Enabled {
		return nil
	}

	var channels []Channel
	if ch := newWebhook(cfg.Webhook); ch != nil {
		channels = append(channels, ch)
	}
	if ch := newTelegram(cfg.Telegram); ch != nil {
		channels = append(channels, ch)
	}
	if ch := newEmail(cfg.Email); ch != nil {
		channels = append(channels, ch)
	}
	if len(channels) == 0 {
		if cfg.Enabled {
			log.Warn().Msg("оповещения включены, но не настроен ни один канал")
		}
		return nil
	}

	severity := model.Severity(cfg.MinSeverity)
	if severity == "" {
		severity = model.SeverityCritical
	}

	n := &Notifier{
		channels:    channels,
		minSeverity: severity,
		log:         log,
		queue:       make(chan Message, queueSize),
		done:        make(chan struct{}),
	}
	n.location.Store(time.UTC)
	n.wg.Add(1)
	go n.run()

	names := make([]string, 0, len(channels))
	for _, ch := range channels {
		names = append(names, ch.Name())
	}
	message := "каналы внешних уведомлений подготовлены"
	if cfg.Enabled {
		message = "оповещения наружу включены"
	}
	log.Info().Strs("каналы", names).Str("порог", string(severity)).Msg(message)
	return n
}

// ChannelNames lists configured delivery mechanisms without exposing their
// credentials through the API.
func (n *Notifier) ChannelNames() []string {
	if n == nil {
		return nil
	}
	out := make([]string, 0, len(n.channels))
	for _, ch := range n.channels {
		out = append(out, ch.Name())
	}
	return out
}

// SendChannel performs one synchronous outbox attempt. Retries and their
// persistence belong to Manager; the transport stays deliberately simple.
func (n *Notifier) SendChannel(ctx context.Context, name string, m Message) error {
	if n == nil {
		return fmt.Errorf("каналы уведомлений не настроены")
	}
	for _, ch := range n.channels {
		if ch.Name() != name {
			continue
		}
		sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
		err := ch.Send(sendCtx, m)
		cancel()
		return err
	}
	return fmt.Errorf("канал %q не настроен", name)
}

// Alert принимает загоревшееся оповещение. Не блокирует: очередь конечна, и
// при переполнении сообщение теряется, а не задерживает того, кто его подал.
func (n *Notifier) Alert(a model.Alert) {
	if n == nil || !n.passes(a.Severity) {
		return
	}
	select {
	case n.queue <- Message{
		Severity: a.Severity,
		Kind:     a.Kind,
		Object:   a.ObjectName,
		Text:     a.Message,
		Details:  a.Details,
		At:       n.now(),
	}:
	default:
		n.mu.Lock()
		n.dropped++
		count := n.dropped
		n.mu.Unlock()
		n.log.Warn().Int("потеряно", count).Msg("очередь оповещений переполнена")
	}
}

// SetTimezone changes the zone used in human-readable notifications. The
// event instant remains unchanged; RFC3339 output carries the selected offset.
func (n *Notifier) SetTimezone(name string) error {
	if n == nil {
		return nil
	}
	loc, err := time.LoadLocation(strings.TrimSpace(name))
	if err != nil {
		return fmt.Errorf("часовой пояс уведомлений %q: %w", name, err)
	}
	n.location.Store(loc)
	return nil
}

func (n *Notifier) now() time.Time {
	if n == nil {
		return time.Now().UTC()
	}
	loc := n.location.Load()
	if loc == nil {
		loc = time.UTC
	}
	return time.Now().In(loc)
}

// passes отсекает то, что ниже порога. Предупреждения по умолчанию наружу не
// уходят: их бывает много, и они не требуют, чтобы человека будили.
func (n *Notifier) passes(s model.Severity) bool {
	if n.minSeverity == model.SeverityCritical {
		return s == model.SeverityCritical
	}
	return true
}

func (n *Notifier) run() {
	defer n.wg.Done()
	for {
		select {
		case m := <-n.queue:
			n.deliver(m)
		case <-n.done:
			// Дочерпываем очередь: сообщения уже приняты, и терять их при
			// остановке незачем.
			for {
				select {
				case m := <-n.queue:
					n.deliver(m)
				default:
					return
				}
			}
		}
	}
}

func (n *Notifier) deliver(m Message) {
	for _, ch := range n.channels {
		ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
		err := ch.Send(ctx, m)
		cancel()
		if err != nil {
			n.log.Warn().Err(err).Str("канал", ch.Name()).Str("объект", m.Object).
				Msg("оповещение не доставлено")
			continue
		}
		n.log.Debug().Str("канал", ch.Name()).Str("объект", m.Object).
			Msg("оповещение отправлено")
	}
}

// Close останавливает отправителя, дав ему дочерпать очередь.
func (n *Notifier) Close() {
	if n == nil {
		return
	}
	n.once.Do(func() {
		close(n.done)
		n.wg.Wait()
	})
}

// subject собирает заголовок, одинаковый для всех каналов.
func subject(m Message) string {
	prefix := "ovirt-backup"
	if m.Severity == model.SeverityCritical {
		prefix = "ovirt-backup: критическое"
	}
	if m.Object == "" {
		return prefix
	}
	return prefix + " — " + m.Object
}

// body собирает текст сообщения.
func body(m Message) string {
	text := m.Text
	if m.Details != "" {
		text += "\n\n" + m.Details
	}
	text += "\n\nтип: " + m.Kind + "\nвремя: " + m.At.Format(time.RFC3339)
	if m.AlertID != "" {
		text += fmt.Sprintf("\nalert_id: %s\nсобытие: %s\nпоследовательность: %d",
			m.AlertID, m.Event, m.Sequence)
	}
	return text
}
