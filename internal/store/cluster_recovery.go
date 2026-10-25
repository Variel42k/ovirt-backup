package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func (s *Store) GetClusterRecoveryPolicy(ctx context.Context, serverID, clusterID string) (model.ClusterRecoveryPolicy, error) {
	p := model.DefaultClusterRecoveryPolicy(serverID, clusterID)
	var raw []byte
	err := s.db.QueryRow(ctx, `SELECT payload FROM cluster_recovery_policies WHERE server_id=? AND cluster_id=?`, serverID, clusterID).Scan(&raw)
	if err == sql.ErrNoRows {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(raw, &p)
	p.ServerID, p.ClusterID, p.Configured = serverID, clusterID, true
	return p, err
}

func (s *Store) SetClusterRecoveryPolicy(ctx context.Context, p model.ClusterRecoveryPolicy, actor string) error {
	if err := p.Validate(); err != nil {
		return err
	}
	p.Configured, p.UpdatedBy, p.UpdatedAt = true, actor, time.Now().UTC()
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO cluster_recovery_policies(server_id,cluster_id,payload) VALUES(?,?,?)
	 ON CONFLICT(server_id,cluster_id) DO UPDATE SET payload=excluded.payload`, p.ServerID, p.ClusterID, string(raw))
	return err
}

func (s *Store) DeleteClusterRecoveryPolicy(ctx context.Context, serverID, clusterID string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM cluster_recovery_policies WHERE server_id=? AND cluster_id=?`, serverID, clusterID)
	return err
}

func (s *Store) ObserveVMRecovery(ctx context.Context, serverID, vmID, reason string, now time.Time) (model.VMRecoveryObservation, error) {
	var o model.VMRecoveryObservation
	err := s.db.QueryRow(ctx, `INSERT INTO vm_recovery_observations(server_id,vm_id,since,failures,reason) VALUES(?,?,?,1,?)
	 ON CONFLICT(server_id,vm_id) DO UPDATE SET failures=vm_recovery_observations.failures+1, reason=excluded.reason
	 RETURNING since,failures,reason`, serverID, vmID, now, reason).Scan(&o.Since, &o.Failures, &o.Reason)
	return o, err
}

func (s *Store) ClearVMRecoveryObservation(ctx context.Context, serverID, vmID string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM vm_recovery_observations WHERE server_id=? AND vm_id=?`, serverID, vmID)
	return err
}

func (s *Store) VMRecoveryBudget(ctx context.Context, serverID, vmID string, since time.Time) (int, time.Time, model.RemediationAction, error) {
	var count int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM remediation_records WHERE server_id=? AND object_id=? AND triggered_by='cluster-recovery' AND status IN ('running','succeeded','failed') AND created_at>=?`, serverID, vmID, since).Scan(&count)
	if err != nil {
		return 0, time.Time{}, "", err
	}
	var at time.Time
	var action model.RemediationAction
	err = s.db.QueryRow(ctx, `SELECT created_at,action FROM remediation_records WHERE server_id=? AND object_id=? AND triggered_by='cluster-recovery' AND status IN ('running','succeeded','failed') ORDER BY created_at DESC LIMIT 1`, serverID, vmID).Scan(&at, &action)
	if err == sql.ErrNoRows {
		err = nil
	}
	return count, at, action, err
}

func (s *Store) VMRecoveryBackupBusy(ctx context.Context, serverID, vmID string) (bool, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM backup_runs WHERE server_id=? AND vm_id=? AND status IN ('pending','running')`, serverID, vmID).Scan(&n)
	return n > 0, err
}
