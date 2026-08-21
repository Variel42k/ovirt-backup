package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/smtp"
	"net/url"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/config"
)

// httpClient общий для webhook и Telegram: свой на канал не нужен, а предел
// времени всё равно задаёт контекст вызова.
var httpClient = &http.Client{Timeout: sendTimeout}

// --- Webhook ---------------------------------------------------------------

type webhookChannel struct {
	url   string
	token string
}

func newWebhook(cfg config.WebhookConfig) Channel {
	if strings.TrimSpace(cfg.URL) == "" {
		return nil
	}
	return &webhookChannel{url: strings.TrimSpace(cfg.URL), token: strings.TrimSpace(cfg.Token)}
}

func (w *webhookChannel) Name() string { return "webhook" }

// Send отправляет оповещение как JSON. Поля названы так же, как в API, чтобы
// принимающая сторона разбирала их тем же кодом.
func (w *webhookChannel) Send(ctx context.Context, m Message) error {
	payload, err := json.Marshal(map[string]any{
		"severity": string(m.Severity),
		"kind":     m.Kind,
		"object":   m.Object,
		"message":  m.Text,
		"details":  m.Details,
		"at":       m.At.Format(time.RFC3339),
		"alert_id": m.AlertID,
		"event":    m.Event,
		"sequence": m.Sequence,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if w.token != "" {
		req.Header.Set("Authorization", "Bearer "+w.token)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Тело читаем и выбрасываем: без этого соединение не переиспользуется.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s ответил %d", w.url, resp.StatusCode)
	}
	return nil
}

// --- Telegram --------------------------------------------------------------

type telegramChannel struct {
	token  string
	chatID string
	// api подменяется в тестах; в бою это адрес Telegram.
	api string
}

func newTelegram(cfg config.TelegramConfig) Channel {
	if strings.TrimSpace(cfg.BotToken) == "" || strings.TrimSpace(cfg.ChatID) == "" {
		return nil
	}
	return &telegramChannel{
		token:  strings.TrimSpace(cfg.BotToken),
		chatID: strings.TrimSpace(cfg.ChatID),
		api:    "https://api.telegram.org",
	}
}

func (t *telegramChannel) Name() string { return "telegram" }

func (t *telegramChannel) Send(ctx context.Context, m Message) error {
	// Текстом формы, а не JSON: так не нужно ни экранирование разметки, ни
	// parse_mode — сообщение уходит как есть, вместе со скобками и кавычками
	// из имён групп и путей.
	form := url.Values{
		"chat_id": {t.chatID},
		"text":    {subject(m) + "\n\n" + body(m)},
	}
	endpoint := t.api + "/bot" + t.token + "/sendMessage"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		// Ответ Telegram содержит причину отказа словами; без неё разбираться
		// пришлось бы наугад. Токена в ответе нет, а адрес с токеном в журнал
		// не попадает.
		return fmt.Errorf("telegram ответил %d: %s", resp.StatusCode, strings.TrimSpace(string(answer)))
	}
	return nil
}

// --- Почта -----------------------------------------------------------------

type emailChannel struct {
	cfg config.EmailConfig
}

func newEmail(cfg config.EmailConfig) Channel {
	if strings.TrimSpace(cfg.SMTPHost) == "" || len(cfg.To) == 0 {
		return nil
	}
	return &emailChannel{cfg: cfg}
}

func (e *emailChannel) Name() string { return "email" }

func (e *emailChannel) Send(ctx context.Context, m Message) error {
	address := fmt.Sprintf("%s:%d", e.cfg.SMTPHost, e.cfg.SMTPPort)

	from := e.cfg.From
	if from == "" {
		from = e.cfg.Username
	}
	// Тема кодируется по RFC 2047: русский заголовок без этого приезжает
	// набором вопросительных знаков у половины почтовых клиентов.
	message := "From: " + from + "\r\n" +
		"To: " + strings.Join(e.cfg.To, ", ") + "\r\n" +
		"Subject: " + mime.BEncoding.Encode("utf-8", subject(m)) + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n" +
		"\r\n" + body(m) + "\r\n"

	var auth smtp.Auth
	if e.cfg.Username != "" {
		auth = smtp.PlainAuth("", e.cfg.Username, e.cfg.Password, e.cfg.SMTPHost)
	}

	// smtp.SendMail не принимает контекст; предел времени обеспечивает
	// http-независимый таймаут вызывающего, а сама отправка идёт в отдельной
	// горутине очереди и опрос не задерживает.
	done := make(chan error, 1)
	go func() { done <- smtp.SendMail(address, auth, from, e.cfg.To, []byte(message)) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("%s не ответил за отведённое время", address)
	}
}
