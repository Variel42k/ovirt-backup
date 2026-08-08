package ovirt

import (
	"context"
	"fmt"
	"time"
)

// An ImageTransfer is the control-plane half of moving disk data: the engine
// picks a host, opens a ticket in ovirt-imageio on it and hands back a URL.
// The data itself is read or written directly against that URL.

// TransferRequest describes a transfer to open.
type TransferRequest struct {
	// Ровно одно из трёх должно быть заполнено.
	DiskID     string // обычная передача по диску (нужно для upload)
	SnapshotID string // image_id диска в снапшоте (бэкап через снапшот)

	// BackupID переводит передачу в режим чтения точки Backup API; вместе с
	// DiskID это единственный способ прочитать данные горячего бэкапа.
	BackupID string

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
}

// CreateTransfer opens an image transfer.
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
		"timeout_policy":     req.TimeoutPolicy,
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
	if req.Shallow {
		body["shallow"] = true
	}

	var transfer ImageTransfer
	if err := c.post(ctx, "/imagetransfers", body, &transfer); err != nil {
		return nil, err
	}
	return &transfer, nil
}

// GetTransfer reads the current state of a transfer.
func (c *Client) GetTransfer(ctx context.Context, id string) (*ImageTransfer, error) {
	var transfer ImageTransfer
	if err := c.get(ctx, "/imagetransfers/"+id, &transfer); err != nil {
		return nil, err
	}
	return &transfer, nil
}

// WaitTransferReady polls until the transfer is usable and a data URL is
// available.
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

// DataURL picks the endpoint to read or write. The direct host URL is faster;
// the engine proxy is the fallback when the backup server cannot reach the
// hypervisors directly, which is common when they sit on an isolated storage
// network.
func DataURL(t *ImageTransfer, preferProxy bool) string {
	if preferProxy && t.ProxyURL != "" {
		return t.ProxyURL
	}
	if t.TransferURL != "" {
		return t.TransferURL
	}
	return t.ProxyURL
}

// FinalizeTransfer completes a transfer successfully.
func (c *Client) FinalizeTransfer(ctx context.Context, id string) error {
	return c.post(ctx, "/imagetransfers/"+id+"/finalize", struct{}{}, nil)
}

// CancelTransfer aborts a transfer and releases the disk.
func (c *Client) CancelTransfer(ctx context.Context, id string) error {
	return c.post(ctx, "/imagetransfers/"+id+"/cancel", struct{}{}, nil)
}

// ExtendTransfer resets the inactivity timer. Long verification passes between
// reads can otherwise let the engine cancel a healthy transfer.
func (c *Client) ExtendTransfer(ctx context.Context, id string) error {
	return c.post(ctx, "/imagetransfers/"+id+"/extend", struct{}{}, nil)
}

// WaitTransferDone polls until a finalised transfer reaches a terminal phase.
func (c *Client) WaitTransferDone(ctx context.Context, id string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		transfer, err := c.GetTransfer(ctx, id)
		if err != nil {
			// Finished transfers are garbage-collected by the engine.
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

// CloseTransfer finalises a transfer, falling back to cancel when finalising is
// rejected. Used in deferred cleanup where the caller cannot handle an error
// but must not leave a disk locked.
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
