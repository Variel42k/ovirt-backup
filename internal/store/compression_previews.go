package store

import (
	"context"
	"encoding/json"
	"time"
)

// В базе сохраняются идентификаторы ресурсов источника. Данные выборки
// и учётные данные в базу не попадают. Блокировка сеанса указывает,
// что за уборку всё ещё отвечает работающий обработчик.
type CompressionPreview struct {
	ID, ServerID, VMID, Status, Error string
	Resources                         json.RawMessage
}

func (s *Store) CreateCompressionPreview(ctx context.Context, p *CompressionPreview) error {
	_, err := s.db.Exec(ctx, `INSERT INTO compression_previews(id,server_id,vm_id,resources,status,created_at) VALUES (?,?,?,?,?,?)`, p.ID, p.ServerID, p.VMID, string(p.Resources), "running", time.Now().UnixMilli())
	return err
}
func (s *Store) SaveCompressionPreview(ctx context.Context, p *CompressionPreview) error {
	_, err := s.db.Exec(ctx, `UPDATE compression_previews SET resources=?,status=?,error=? WHERE id=?`, string(p.Resources), p.Status, p.Error, p.ID)
	return err
}
func (s *Store) OpenCompressionPreviews(ctx context.Context) ([]*CompressionPreview, error) {
	rows, err := s.db.Query(ctx, `SELECT id,server_id,vm_id,resources,status,error FROM compression_previews WHERE status IN ('running','cleanup_failed') ORDER BY created_at LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CompressionPreview
	for rows.Next() {
		p := &CompressionPreview{}
		var raw string
		if err = rows.Scan(&p.ID, &p.ServerID, &p.VMID, &raw, &p.Status, &p.Error); err != nil {
			return nil, err
		}
		p.Resources = json.RawMessage(raw)
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Store) TryCompressionPreviewLock(ctx context.Context, id string) (*VMTransferLock, error) {
	return s.tryRecoveryLock(ctx, "jhvirt-compression-preview:"+id)
}
func (s *Store) VMCompressionPreviewBusy(ctx context.Context, serverID, vmID string) (bool, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM compression_previews WHERE server_id=? AND vm_id=? AND status IN ('running','cleanup_failed')`, serverID, vmID).Scan(&n)
	return n > 0, err
}
