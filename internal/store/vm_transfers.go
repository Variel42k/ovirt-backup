package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func (s *Store) GetVMTransfer(ctx context.Context, id string) (*model.VMTransfer, error) {
	var raw []byte
	if err := s.db.QueryRow(ctx, `SELECT payload FROM vm_transfers WHERE id=?`, id).Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var t model.VMTransfer
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Store) ListVMTransfers(ctx context.Context) ([]*model.VMTransfer, error) {
	rows, err := s.db.Query(ctx, `SELECT payload FROM vm_transfers ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.VMTransfer{}
	for rows.Next() {
		var raw []byte
		var t model.VMTransfer
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &t); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

func (s *Store) CreateVMTransfer(ctx context.Context, t *model.VMTransfer) error {
	return s.CreateVMTransfers(ctx, []*model.VMTransfer{t})
}

// CreateVMTransfers reserves every source atomically before any worker starts.
func (s *Store) CreateVMTransfers(ctx context.Context, items []*model.VMTransfer) error {
	ordered := append([]*model.VMTransfer(nil), items...)
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i].Request, ordered[j].Request
		return a.SourceServerID+":"+a.SourceVMID < b.SourceServerID+":"+b.SourceVMID
	})
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		for _, t := range ordered {
			key := t.Request.SourceServerID + ":" + t.Request.SourceVMID
			if _, err := tx.ExecContext(ctx, s.db.Rebind(`SELECT pg_advisory_xact_lock(hashtextextended(?,0))`), key); err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		for _, t := range items {
			var n int
			if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT count(*) FROM backup_runs WHERE server_id=? AND vm_id=? AND status IN ('pending','running')`), t.Request.SourceServerID, t.Request.SourceVMID).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return fmt.Errorf("ВМ занята резервным копированием")
			}
			if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT count(*) FROM vm_transfers WHERE ((source_server_id=? AND source_vm_id=? AND status NOT IN ('canceled','rolled_back')) OR (target_server_id=? AND target_vm_id=? AND status NOT IN ('canceled','rolled_back','completed')))`), t.Request.SourceServerID, t.Request.SourceVMID, t.Request.SourceServerID, t.Request.SourceVMID).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return fmt.Errorf("ВМ уже участвует в миграции")
			}
			t.CreatedAt = now
			t.UpdatedAt = now
			raw, err := json.Marshal(t)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO vm_transfers(id,source_server_id,source_vm_id,target_server_id,storage_target_id,status,payload) VALUES(?,?,?,?,?,?,?)`), t.ID, t.Request.SourceServerID, t.Request.SourceVMID, t.Request.TargetServerID, t.Request.StorageTargetID, t.Status, string(raw)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) UpdateVMTransfer(ctx context.Context, t *model.VMTransfer) error {
	t.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `UPDATE vm_transfers SET status=?,target_vm_id=?,payload=?,updated_at=? WHERE id=?`, t.Status, t.TargetVMID, string(raw), t.UpdatedAt, t.ID)
	return err
}

// ClaimVMTransfer makes duplicate action requests fail before either worker
// contacts a hypervisor, including requests handled by another API process.
func (s *Store) ClaimVMTransfer(ctx context.Context, t *model.VMTransfer, expected string) error {
	t.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	result, err := s.db.Exec(ctx, `UPDATE vm_transfers SET status=?,payload=?,updated_at=? WHERE id=? AND status=?`, t.Status, string(raw), t.UpdatedAt, t.ID, expected)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("состояние миграции изменилось; обновите страницу")
	}
	return nil
}

// VMTransferOwner locks source VMs until cancellation/rollback. Completed source
// VMs must stay off while their replacement is serving traffic.
func (s *Store) VMTransferOwner(ctx context.Context, serverID, vmID string) (string, error) {
	var id string
	err := s.db.QueryRow(ctx, `SELECT id FROM vm_transfers WHERE
 ((source_server_id=? AND source_vm_id=? AND status NOT IN ('canceled','rolled_back')) OR
 (target_server_id=? AND target_vm_id=? AND status NOT IN ('canceled','rolled_back','completed')))
 LIMIT 1`, serverID, vmID, serverID, vmID).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

func (s *Store) VMTransferProtectsRun(ctx context.Context, run *model.BackupRun) error {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM vm_transfers WHERE source_server_id=? AND source_vm_id=? AND status NOT IN ('canceled','rolled_back','completed')`, run.ServerID, run.VMID).Scan(&n)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("копия защищена незавершённой миграцией виртуальной машины")
	}
	return nil
}

func (s *Store) VMTransferServerBusy(ctx context.Context, serverID string) (bool, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM vm_transfers WHERE (source_server_id=? OR target_server_id=?) AND status NOT IN ('canceled','rolled_back','completed')`, serverID, serverID).Scan(&n)
	return n > 0, err
}

func (s *Store) VMTransferRepositoryReferenced(ctx context.Context, id string, activeOnly bool) (bool, error) {
	query := `SELECT count(*) FROM vm_transfers WHERE storage_target_id=?`
	if activeOnly {
		query += ` AND status NOT IN ('canceled','rolled_back','completed')`
	}
	var n int
	err := s.db.QueryRow(ctx, query, id).Scan(&n)
	return n > 0, err
}

func (s *Store) DeleteReleasedVMTransfer(ctx context.Context, id string) error {
	result, err := s.db.Exec(ctx, `DELETE FROM vm_transfers WHERE id=? AND status IN ('canceled','rolled_back')`, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("удалить запись можно только после отмены или отката")
	}
	return nil
}
