package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// VMTransferSamples are bounded per operation and survive browser reloads.
func (s *Store) VMTransferSamples(ctx context.Context, id string) ([]model.VMTransferSample, error) {
	rows, err := s.db.Query(ctx, `SELECT payload FROM (SELECT at,payload FROM vm_transfer_samples WHERE transfer_id=? ORDER BY at DESC LIMIT 600) recent ORDER BY at`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.VMTransferSample{}
	for rows.Next() {
		var raw []byte
		var sample model.VMTransferSample
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &sample); err != nil {
			return nil, err
		}
		out = append(out, sample)
	}
	return out, rows.Err()
}

func (s *Store) RecordVMTransferSample(ctx context.Context, t *model.VMTransfer) error {
	sample := model.VMTransferSample{At: time.Now().UTC(), Phase: t.Phase, StageID: t.Phase, Direction: "control", Progress: t.Progress, Bytes: t.TransferredBytes, Total: t.TotalBytes}
	if t.Status == "running" {
		if t.Phase == "backup" || t.Phase == "copying_changes" {
			if n := len(t.RunIDs); n > 0 {
				if run, e := s.GetBackupRun(ctx, t.RunIDs[n-1]); e == nil && (run.Status == model.RunRunning || t.Phase == "backup") {
					sample.StageID = "backup:" + run.ID
					sample.Direction = "source_to_repository"
					sample.Bytes = run.ReadBytes
					sample.Total = run.LogicalBytes
					sample.Progress = run.Progress
				}
			}
		}
		if t.Phase == "restoring_target" && t.RestoreID != "" {
			if run, e := s.GetRestoreRun(ctx, t.RestoreID); e == nil {
				sample.StageID = "restore:" + run.ID
				sample.Direction = "repository_to_target"
				sample.Bytes = run.TransferredBytes
				sample.Total = run.TotalBytes
				sample.Speed = run.BytesPerSecond
				sample.Progress = run.Progress
			}
		}
		if t.Phase == "copying_changes" && sample.Direction == "control" {
			sample.StageID = "delta:" + t.AppliedRunID
			sample.Direction = "repository_to_target"
			sample.Speed = t.BytesPerSecond
		}
	}
	previous, err := s.VMTransferSamples(ctx, t.ID)
	if err != nil {
		return err
	}
	if len(previous) > 0 {
		last := previous[len(previous)-1]
		if t.Status != "running" {
			sample.StageID, sample.Bytes, sample.Total, sample.Direction = last.StageID, last.Bytes, last.Total, last.Direction
		} else if last.StageID == sample.StageID && sample.Bytes >= last.Bytes {
			elapsed := sample.At.Sub(last.At).Seconds()
			if elapsed > 0 {
				sample.Speed = int64(float64(sample.Bytes-last.Bytes) / elapsed)
			}
		}
	}
	raw, err := json.Marshal(sample)
	if err != nil {
		return err
	}
	if _, err = s.db.Exec(ctx, `INSERT INTO vm_transfer_samples(transfer_id,at,payload) VALUES(?,?,?)`, t.ID, sample.At, string(raw)); err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `DELETE FROM vm_transfer_samples WHERE transfer_id=? AND at NOT IN (SELECT at FROM vm_transfer_samples WHERE transfer_id=? ORDER BY at DESC LIMIT 600)`, t.ID, t.ID)
	return err
}
