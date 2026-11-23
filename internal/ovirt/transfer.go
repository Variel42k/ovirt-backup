package ovirt

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// ImageTransfer — половина контрольной плоскости движущихся данных диска: движок выбирает
// хост, открывает на нем билет в ovirt-imageio и передает назад URL.
// Сами данные читаются или пишутся прямо против этого URL.

// TransferRequest описывает переход к открытию.
type TransferRequest struct {
	// Ровно одно из трёх должно быть заполнено.
	DiskID     string // обычная передача по диску (нужно для upload)
	SnapshotID string // image_id диска в снапшоте (бэкап через снапшот)

	// BackupID переводит передачу в режим чтения точки Backup API; вместе с
	// DiskID это единственный способ прочитать данные горячего бэкапа.
	BackupID string
	// HostID выводит ImageIO на выбранный хост загрузки, включая oVirt 4.3.
	HostID string

	Direction string // download | upload
	// raw отдаёт гостевые данные как есть — то, что нужно для бэкапа.
	Format string

	InactivityTimeout time.Duration
	// TimeoutPolicy: legacy | pause | cancel. cancel освобождает диск сам,
	// если наш процесс умер посреди передачи.
	TimeoutPolicy string

	// Shallow читает только верхний слой цепочки образов (используется при
	// покотором бэкапе снапшотов).
	Shallow bool

	// RequestTimeout ограничивает плоскость управления POST, независимо от короткого
	// инвентарного таймаута. Подготовка загрузки может включать медленную активацию
	// хранилища и создание билета на хосте.
	RequestTimeout time.Duration
}

// CreateTransfer открывается передача образа.
func (c *Client) CreateTransfer(ctx context.Context, req TransferRequest) (*ImageTransfer, error) {
	if req.Direction == "" {
		req.Direction = "download"
	}
	if req.Format == "" {
		req.Format = "raw"
	}
	if req.TimeoutPolicy == "" {
		req.TimeoutPolicy = "cancel"
	}
	if req.InactivityTimeout <= 0 {
		req.InactivityTimeout = 60 * time.Second
	}

	body := map[string]any{
		"direction":          req.Direction,
		"format":             req.Format,
		"inactivity_timeout": int(req.InactivityTimeout / time.Second),
	}
	legacyRequest := c.legacyImageTransfer.Load()
	if !legacyRequest {
		body["timeout_policy"] = req.TimeoutPolicy
	}
	switch {
	case req.SnapshotID != "":
		body["snapshot"] = map[string]string{"id": req.SnapshotID}
	case req.DiskID != "":
		body["disk"] = map[string]string{"id": req.DiskID}
	default:
		return nil, fmt.Errorf("для передачи образа нужен идентификатор диска или снапшота")
	}
	if req.BackupID != "" {
		body["backup"] = map[string]string{"id": req.BackupID}
	}
	if req.HostID != "" {
		body["host"] = map[string]string{"id": req.HostID}
	}
	if req.Shallow {
		body["shallow"] = true
	}

	var transfer ImageTransfer
	requestTimeout := req.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = max(2*time.Minute, c.http.Timeout)
	}
	longRequest := func(o *requestOptions) { o.timeout = requestTimeout }
	err := c.post(ctx, "/imagetransfers", body, &transfer, longRequest)
	if err == nil {
		return &transfer, nil
	}

	// timeout_policy и режим cancel появились в oVirt 4.4. Движок 4.3 отклоняет весь запрос
	// ImageTransfer с общим сообщением "For correct usage". Такой POST не открывает передачу,
	// поэтому повторяем запрос в формате 4.3 и запоминаем этот формат для остальных дисков.
	var apiErr *APIError
	if !legacyRequest && errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest {
		delete(body, "timeout_policy")
		transfer = ImageTransfer{}
		if retryErr := c.post(ctx, "/imagetransfers", body, &transfer, longRequest); retryErr != nil {
			// Конфликт означает, что 4,3-образный документ был понят, и только состояние
			// диска отклонило его. Запомните понижение сейчас; В противном случае каждая
			// повторная попытка блокировки сначала отправит другой известный неверный
			// запрос 4.4.
			if IsConflict(retryErr) {
				c.legacyImageTransfer.Store(true)
			}
			return nil, retryErr
		}
		c.legacyImageTransfer.Store(true)
		return &transfer, nil
	}
	return nil, err
}

// CreateUploadForNewDisk может восстановить потерянный POST ответ для диска, созданного
// исключительно этой операцией. Никогда не используйте его для существующего диска: сеанс
// другого загрузчика не должен быть принят. Сама POST никогда не повторяется.
func (c *Client) CreateUploadForNewDisk(ctx context.Context, req TransferRequest) (*ImageTransfer, error) {
	if req.DiskID == "" || req.Direction != "upload" || req.SnapshotID != "" || req.BackupID != "" {
		return nil, fmt.Errorf("восстановление ответа передачи допустимо только для загрузки в новый диск")
	}
	transfer, err := c.CreateTransfer(ctx, req)
	if err == nil {
		return transfer, nil
	}
	var networkErr net.Error
	if ctx.Err() != nil || !errors.As(err, &networkErr) {
		return nil, err
	}

	// Движок может завершить запрос после истечения таймаута HTTP-клиента.
	// Найдите эту сессию по ее эксклюзивному идентификатору диска, включая ссылку на образ
	// 4.3, а не создавайте дубликат сессии.
	lookupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		all, lookupErr := c.ListImageTransfers(lookupCtx)
		if lookupErr != nil {
			return nil, fmt.Errorf("%w; состояние передачи диска %s не удалось проверить: %v", err, req.DiskID, lookupErr)
		}
		var found *ImageTransfer
		for _, candidate := range all {
			if candidate.Terminal() || candidate.Direction != "upload" ||
				(candidate.Disk.ID != req.DiskID && candidate.Image.ID != req.DiskID) {
				continue
			}
			if req.HostID != "" && candidate.Host.ID != "" && candidate.Host.ID != req.HostID {
				return nil, fmt.Errorf("%w; передача нового диска открыта на другом хосте %s, выбран %s", err, candidate.Host.ID, req.HostID)
			}
			if found != nil {
				return nil, fmt.Errorf("%w; у нового диска %s несколько передач — нельзя однозначно продолжить", err, req.DiskID)
			}
			copy := candidate
			found = &copy
		}
		if found != nil {
			c.log.Warn().Str("диск-id", req.DiskID).Str("transfer", found.ID).Str("фаза", found.Phase).
				Msg("ответ открытия передачи потерян, найдена передача нового диска; продолжаю без повторного POST")
			return found, nil
		}
		select {
		case <-lookupCtx.Done():
			return nil, fmt.Errorf("%w; передача нового диска %s не найдена, проверьте ImageTransfer в движке", err, req.DiskID)
		case <-time.After(time.Second):
		}
	}
}

// CreateTransferWhenReady открывает передачу после очистки переходных дисковых блокировок.
// Старые движки oVirt создают снапшот и завершают передачу асинхронно. Снапшот может уже
// быть "ok", а POST /imagetransfers ещё несколько секунд отвечает 409. Повторяем только
// конфликты. Остальные ошибки возвращаются сразу.
func (c *Client) CreateTransferWhenReady(ctx context.Context, req TransferRequest,
	timeout time.Duration) (*ImageTransfer, error) {
	return c.createTransferWhenReady(ctx, req, timeout, 5*time.Second)
}

func (c *Client) createTransferWhenReady(ctx context.Context, req TransferRequest,
	timeout, interval time.Duration) (*ImageTransfer, error) {
	var transfer *ImageTransfer
	err := retryConflict(ctx, timeout, interval, func() error {
		var err error
		transfer, err = c.CreateTransfer(ctx, req)
		return err
	})
	if err != nil {
		return nil, err
	}
	return transfer, nil
}

// GetTransfer прочита́етъ текущій состояніе перемѣны.
func (c *Client) GetTransfer(ctx context.Context, id string) (*ImageTransfer, error) {
	var transfer ImageTransfer
	if err := c.get(ctx, "/imagetransfers/"+id, &transfer); err != nil {
		return nil, err
	}
	return &transfer, nil
}

// WaitTransferReady опрашивает передачу, пока она не будет готова и не появится URL данных.
func (c *Client) WaitTransferReady(ctx context.Context, id string, timeout time.Duration) (*ImageTransfer, error) {
	deadline := time.Now().Add(timeout)
	for {
		transfer, err := c.GetTransfer(ctx, id)
		if err != nil {
			return nil, err
		}
		switch transfer.Phase {
		case "transferring":
			if transfer.TransferURL == "" && transfer.ProxyURL == "" {
				return nil, fmt.Errorf("передача %s активна, но движок не сообщил URL данных", id)
			}
			return transfer, nil
		case "finished_failure", "cancelled", "unknown":
			return nil, fmt.Errorf("передача %s завершилась в состоянии %s", id, transfer.Phase)
		case "paused_system", "paused_user":
			return nil, fmt.Errorf("передача %s приостановлена (%s)", id, transfer.Phase)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("передача %s не стала активной за %s (состояние: %s)",
				id, timeout, transfer.Phase)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// DataURL выбирает адрес чтения или записи. Предпочитаем прямой адрес хоста. Если сервер
// копий не видит гипервизор в изолированной сети хранения, используем прокси движка.
func DataURL(t *ImageTransfer, preferProxy bool) string {
	if preferProxy && t.ProxyURL != "" {
		return t.ProxyURL
	}
	if t.TransferURL != "" {
		return t.TransferURL
	}
	return t.ProxyURL
}

// FinalizeTransfer успешно завершается передача.
func (c *Client) FinalizeTransfer(ctx context.Context, id string) error {
	return c.post(ctx, "/imagetransfers/"+id+"/finalize", struct{}{}, nil)
}

// CancelTransfer прерывает передачу и освобождает диск.
func (c *Client) CancelTransfer(ctx context.Context, id string) error {
	return c.post(ctx, "/imagetransfers/"+id+"/cancel", struct{}{}, nil)
}

// ExtendTransfer сбрасывает таймер бездействия. Длинные проверочные проходы между чтениями
// могут в противном случае позволить движку отменить здоровую передачу.
func (c *Client) ExtendTransfer(ctx context.Context, id string) error {
	return c.post(ctx, "/imagetransfers/"+id+"/extend", struct{}{}, nil)
}

// WaitTransferDone опрашивает передачу до перехода в конечное состояние после завершения.
func (c *Client) WaitTransferDone(ctx context.Context, id string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		transfer, err := c.GetTransfer(ctx, id)
		if err != nil {
			// Готовые передачи собираются движком.
			if IsNotFound(err) {
				return "finished_success", nil
			}
			return "", err
		}
		if transfer.Terminal() {
			return transfer.Phase, nil
		}
		if time.Now().After(deadline) {
			return transfer.Phase, fmt.Errorf("передача %s не завершилась за %s (состояние: %s)",
				id, timeout, transfer.Phase)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// FinalizeTransferAndWait завершает загрузку и ждет, пока движок фактически выпустит диск.
// Успешный POST /finalize это только принятая просьба; присоединение диска до того, как
// конечная фаза загоняет его блокировку движка.
func (c *Client) FinalizeTransferAndWait(ctx context.Context, id string, timeout time.Duration) error {
	if err := c.FinalizeTransfer(ctx, id); err != nil {
		return err
	}
	phase, err := c.WaitTransferDone(ctx, id, timeout)
	if err != nil {
		return err
	}
	if phase != "finished_success" {
		return fmt.Errorf("передача %s завершилась в состоянии %s", id, phase)
	}
	return nil
}

// CloseTransfer завершает передачу, отступая назад, чтобы отменить, когда финал
// отклоняется. Используется при отложенной очистке, когда код вызова не может обработать
// ошибку, но не должен оставлять диск заблокированным.
func (c *Client) CloseTransfer(ctx context.Context, id string, success bool) error {
	if success {
		if err := c.FinalizeTransfer(ctx, id); err == nil {
			return nil
		}
	}
	if err := c.CancelTransfer(ctx, id); err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}
