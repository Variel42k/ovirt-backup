package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Обновления и отключение мониторинга не должны скрывать уже обнаруженные домены.
// Использует только сохраненный инвентарь, не связываясь с каким-либо движком.
func (s *Store) SyncKnownDomainRepositories(ctx context.Context) error {
	servers, err := s.ListServers(ctx)
	if err != nil {
		return err
	}
	for _, server := range servers {
		if err = s.SyncDomainRepositories(ctx, server.ID); err != nil {
			return err
		}
	}
	return nil
}

// Инвентаризация предоставляет активные oVirt домены данных в качестве резервных адресатов.
// При этом регистрируются только метаданные; диск не выделяется до тех пор, пока кто-то не
// запишет резервную копию.
func (s *Store) SyncDomainRepositories(ctx context.Context, serverID string) error {
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		var name, kind string
		var enabled bool
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT name,kind,enabled FROM servers WHERE id=? FOR UPDATE`), serverID).Scan(&name, &kind, &enabled); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, s.db.Rebind(`SELECT id,storage_domain_id,prefix FROM storage_targets WHERE kind=? AND server_id=?`), string(model.StorageDomainRepository), serverID)
		if err != nil {
			return err
		}
		var automaticIDs []string
		manualDefaults := map[string]bool{}
		for rows.Next() {
			var id, domainID, prefix string
			if err = rows.Scan(&id, &domainID, &prefix); err != nil {
				rows.Close()
				return err
			}
			if id == model.AutomaticDomainStorageID(serverID, domainID) {
				automaticIDs = append(automaticIDs, id)
			} else if prefix == "backups" {
				manualDefaults[domainID] = true
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range automaticIDs {
			if _, err = tx.ExecContext(ctx, s.db.Rebind(`UPDATE storage_targets SET enabled=false WHERE id=?`), id); err != nil {
				return err
			}
		}
		if !enabled || !model.ServerKind(kind).UsesOVirtAPI() {
			return nil
		}
		rows, err = tx.QueryContext(ctx, s.db.Rebind(`SELECT id,name,available_size,used_size FROM storage_domains WHERE server_id=? AND type='data' AND status='active' ORDER BY id`), serverID)
		if err != nil {
			return err
		}
		type domain struct {
			id, name   string
			free, used int64
		}
		var domains []domain
		for rows.Next() {
			var d domain
			if err = rows.Scan(&d.id, &d.name, &d.free, &d.used); err != nil {
				rows.Close()
				return err
			}
			domains = append(domains, d)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, d := range domains {
			// Повторное использование существующих репозиториев по умолчанию без изменения
			// их идентификаторов, пространств имен или выбора пользователя для отключения
			// ручного хранилища.
			if manualDefaults[d.id] {
				continue
			}
			id := model.AutomaticDomainStorageID(serverID, d.id)
			title := fmt.Sprintf("%s / %s · %s", name, d.name, id[:8])
			now := time.Now().UTC()
			_, err = tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO storage_targets
				(id,name,kind,enabled,server_id,storage_domain_id,prefix,free_bytes,used_bytes,created_at,updated_at)
				VALUES (?,?,?,true,?,?,'backups',?,?,?,?)
				ON CONFLICT (id) DO UPDATE SET name=excluded.name,enabled=true,
				free_bytes=excluded.free_bytes,used_bytes=excluded.used_bytes,updated_at=excluded.updated_at`),
				id, title, string(model.StorageDomainRepository), serverID, d.id, d.free, d.used, now, now)
			if err != nil {
				return fmt.Errorf("домен %s как назначение бэкапа: %w", d.name, err)
			}
		}
		return nil
	})
}
