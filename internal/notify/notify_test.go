package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Порог по умолчанию — критические. Предупреждений на живой установке идут
// десятки, и канал, в который сыплется поток «в целом всё в порядке»,
// перестают читать целиком — вместе с тем единственным сообщением, ради
// которого он заводился.
func TestOnlyCriticalPassesByDefault(t *testing.T) {
	n := &Notifier{minSeverity: model.SeverityCritical}
	if !n.passes(model.SeverityCritical) {
		t.Error("критическое не прошло порог")
	}
	if n.passes(model.SeverityWarning) {
		t.Error("предупреждение прошло при пороге critical")
	}

	n.minSeverity = model.SeverityWarning
	if !n.passes(model.SeverityWarning) || !n.passes(model.SeverityCritical) {
		t.Error("при пороге warning должно проходить и то, и другое")
	}
}

// Отправитель не имеет права задерживать того, кто подал оповещение: очередь
// наполняет монитор, а он опрашивает гипервизоры. Переполнение теряет
// сообщение и пишет об этом в журнал — но не останавливает опрос.
func TestQueueOverflowDoesNotBlock(t *testing.T) {
	n := &Notifier{
		minSeverity: model.SeverityCritical,
		queue:       make(chan Message, 2),
		log:         zerolog.Nop(),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			n.Alert(model.Alert{Severity: model.SeverityCritical, Kind: "test", ObjectName: "vm"})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Alert заблокировался на полной очереди")
	}

	n.mu.Lock()
	dropped := n.dropped
	n.mu.Unlock()
	if dropped == 0 {
		t.Error("переполнение не зафиксировано: потери должны быть видны в журнале")
	}
}

// Канал, который отвечает ошибкой, не должен мешать остальным: у оповещения
// обычно несколько получателей, и недоступный SMTP не повод молчать в Telegram.
type failingChannel struct{ called bool }

func (f *failingChannel) Name() string { return "падающий" }
func (f *failingChannel) Send(context.Context, Message) error {
	f.called = true
	return io.ErrUnexpectedEOF
}

type recordingChannel struct {
	mu   sync.Mutex
	sent []Message
}

func (r *recordingChannel) Name() string { return "запоминающий" }
func (r *recordingChannel) Send(_ context.Context, m Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, m)
	return nil
}

func TestBrokenChannelDoesNotStopTheRest(t *testing.T) {
	broken := &failingChannel{}
	good := &recordingChannel{}
	n := &Notifier{channels: []Channel{broken, good}, log: zerolog.Nop()}

	n.deliver(Message{Severity: model.SeverityCritical, Kind: "test", Object: "vm", Text: "беда"})

	if !broken.called {
		t.Error("падающий канал не вызывался")
	}
	good.mu.Lock()
	defer good.mu.Unlock()
	if len(good.sent) != 1 {
		t.Fatalf("исправный канал получил %d сообщений, ожидалось 1", len(good.sent))
	}
}

func TestWebhookSendsJSON(t *testing.T) {
	type payload struct {
		Severity string `json:"severity"`
		Kind     string `json:"kind"`
		Object   string `json:"object"`
		Message  string `json:"message"`
		AlertID  string `json:"alert_id"`
		Event    string `json:"event"`
		Sequence int    `json:"sequence"`
	}
	got := make(chan payload, 1)
	auth := make(chan string, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth <- r.Header.Get("Authorization")
		var p payload
		_ = json.NewDecoder(r.Body).Decode(&p)
		got <- p
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	channel := newWebhook(config.WebhookConfig{URL: server.URL, Token: "секрет"})
	if channel == nil {
		t.Fatal("канал не собрался при заданном адресе")
	}
	err := channel.Send(context.Background(), Message{
		Severity: model.SeverityCritical, Kind: "backup_failed",
		Object: "db-01", Text: "копия не удалась", At: time.Now(), AlertID: "alert-1",
		Event: model.NotificationReminder, Sequence: 2,
	})
	if err != nil {
		t.Fatalf("отправка: %v", err)
	}

	if header := <-auth; header != "Bearer секрет" {
		t.Errorf("заголовок авторизации %q", header)
	}
	p := <-got
	if p.Severity != "critical" || p.Kind != "backup_failed" || p.Object != "db-01" {
		t.Errorf("получено %+v", p)
	}
	if p.AlertID != "alert-1" || p.Event != "reminder" || p.Sequence != 2 {
		t.Errorf("идентификатор дедупликации потерян: %+v", p)
	}
}

// Отказ приёмника — это ошибка доставки, а не успех: иначе неверно настроенный
// webhook выглядел бы работающим.
func TestWebhookReportsBadStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	channel := newWebhook(config.WebhookConfig{URL: server.URL})
	if err := channel.Send(context.Background(), Message{Kind: "test"}); err == nil {
		t.Error("код 500 принят за успешную доставку")
	}
}

func TestTelegramSendsText(t *testing.T) {
	got := make(chan string, 1)
	path := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		path <- r.URL.Path
		got <- r.PostForm.Get("text")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	channel := newTelegram(config.TelegramConfig{BotToken: "12345:abc", ChatID: "-100"})
	channel.(*telegramChannel).api = server.URL

	err := channel.Send(context.Background(), Message{
		Severity: model.SeverityCritical, Kind: "storage_target_unreachable",
		Object: "хранилище «архив»", Text: "хранилище недоступно", At: time.Now(),
	})
	if err != nil {
		t.Fatalf("отправка: %v", err)
	}

	if p := <-path; p != "/bot12345:abc/sendMessage" {
		t.Errorf("адрес %q", p)
	}
	// Кавычки-ёлочки в именах объектов — обычное дело, и разметка Telegram их
	// не должна ломать: parse_mode не задаётся намеренно.
	text := <-got
	if !strings.Contains(text, "хранилище «архив»") || !strings.Contains(text, "storage_target_unreachable") {
		t.Errorf("текст сообщения: %q", text)
	}
}

// Включённые оповещения без единого настроенного канала — это молчание,
// выглядящее как работающая доставка.
func TestNoChannelsMeansNoNotifier(t *testing.T) {
	if n := New(config.NotificationsConfig{Enabled: true}, zerolog.Nop()); n != nil {
		n.Close()
		t.Error("отправитель собрался без единого канала")
	}
	if n := New(config.NotificationsConfig{Enabled: false,
		Webhook: config.WebhookConfig{URL: "https://example.org"}}, zerolog.Nop()); n != nil {
		n.Close()
		t.Error("отправитель собрался при выключенных оповещениях")
	}
}

// Nil-отправитель обязан молчать, а не падать: он и есть штатное состояние
// установки, где оповещения наружу не настроены.
func TestNilNotifierIsSafe(t *testing.T) {
	var n *Notifier
	n.Alert(model.Alert{Severity: model.SeverityCritical})
	n.Close()
}
