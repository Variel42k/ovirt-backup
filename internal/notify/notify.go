// Package notify доставляет критические оповещения наружу: почтой, webhook-ом
// и в Telegram.
//
// Наружу — потому что внутрь их уже видно. Оповещение, о котором узнаёт только
// тот, кто в этот момент смотрит в интерфейс, ночью не работает, а именно
// ночью и идут бэкапы.
package notify

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"adveng/jh_virt/internal/config"
	"adveng/jh_virt/internal/model"
)

// Message — то, что уходит человеку.
type Message struct {
	Severity model.Severity
	Kind     string
	Object   string
	Text     string
	Details  string
	At       time.Time
}

// Channel — способ доставки. Ошибку возвращает, чтобы её записали в журнал:
// отправитель не пытается повторять — оповещение живёт в системе и никуда не
// денется, а очередь повторов породила бы лавину при недоступном SMTP.
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

	queue chan Message
	wg    sync.WaitGroup
	once  sync.Once
	done  chan struct{}

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
	if !cfg.Enabled {
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
		log.Warn().Msg("оповещения включены, но не настроен ни один канал")
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
	n.wg.Add(1)
	go n.run()

	names := make([]string, 0, len(channels))
	for _, ch := range channels {
		names = append(names, ch.Name())
	}
	log.Info().Strs("каналы", names).Str("порог", string(severity)).
		Msg("оповещения наружу включены")
	return n
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
		At:       time.Now().UTC(),
	}:
	default:
		n.mu.Lock()
		n.dropped++
		count := n.dropped
		n.mu.Unlock()
		n.log.Warn().Int("потеряно", count).Msg("очередь оповещений переполнена")
	}
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
	return text + "\n\nтип: " + m.Kind + "\nвремя: " + m.At.Format(time.RFC3339)
}
