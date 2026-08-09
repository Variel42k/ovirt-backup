-- Время в базе: BIGINT-миллисекунды -> TIMESTAMPTZ.
--
-- Миллисекунды были компромиссом ради SQLite: у него нет типа времени, и
-- единственный способ хранить момент одинаково в обеих СУБД — целое число.
-- СУБД осталась одна, и компромисс больше ничего не покупает, а платить за
-- него приходится дважды: 71 место в коде конвертирует туда-обратно, и любой
-- взгляд в базу глазами упирается в 1754472345123 вместо даты.
--
-- Пересчёт: to_timestamp принимает секунды с дробной частью, поэтому делим на
-- 1000.0, а не на 1000 — целочисленное деление отбросило бы миллисекунды.
-- NULL остаётся NULL, ограничение NOT NULL сохраняется при смене типа.
--
-- Часовой пояс: значения записаны как UTC, TIMESTAMPTZ хранит момент времени
-- без привязки к поясу, поэтому пересчёт точный и обратимый.

ALTER TABLE alerts ALTER COLUMN acked_at TYPE TIMESTAMPTZ USING to_timestamp(acked_at / 1000.0);
ALTER TABLE alerts ALTER COLUMN first_seen TYPE TIMESTAMPTZ USING to_timestamp(first_seen / 1000.0);
ALTER TABLE alerts ALTER COLUMN last_seen TYPE TIMESTAMPTZ USING to_timestamp(last_seen / 1000.0);
ALTER TABLE alerts ALTER COLUMN resolved_at TYPE TIMESTAMPTZ USING to_timestamp(resolved_at / 1000.0);
ALTER TABLE audit_log ALTER COLUMN at TYPE TIMESTAMPTZ USING to_timestamp(at / 1000.0);
ALTER TABLE backup_jobs ALTER COLUMN created_at TYPE TIMESTAMPTZ USING to_timestamp(created_at / 1000.0);
ALTER TABLE backup_jobs ALTER COLUMN last_run_at TYPE TIMESTAMPTZ USING to_timestamp(last_run_at / 1000.0);
ALTER TABLE backup_jobs ALTER COLUMN next_run_at TYPE TIMESTAMPTZ USING to_timestamp(next_run_at / 1000.0);
ALTER TABLE backup_jobs ALTER COLUMN updated_at TYPE TIMESTAMPTZ USING to_timestamp(updated_at / 1000.0);
ALTER TABLE backup_runs ALTER COLUMN created_at TYPE TIMESTAMPTZ USING to_timestamp(created_at / 1000.0);
ALTER TABLE backup_runs ALTER COLUMN ended_at TYPE TIMESTAMPTZ USING to_timestamp(ended_at / 1000.0);
ALTER TABLE backup_runs ALTER COLUMN expires_at TYPE TIMESTAMPTZ USING to_timestamp(expires_at / 1000.0);
ALTER TABLE backup_runs ALTER COLUMN started_at TYPE TIMESTAMPTZ USING to_timestamp(started_at / 1000.0);
ALTER TABLE backup_runs ALTER COLUMN verified_at TYPE TIMESTAMPTZ USING to_timestamp(verified_at / 1000.0);
ALTER TABLE clusters ALTER COLUMN seen_at TYPE TIMESTAMPTZ USING to_timestamp(seen_at / 1000.0);
ALTER TABLE disk_samples ALTER COLUMN at TYPE TIMESTAMPTZ USING to_timestamp(at / 1000.0);
ALTER TABLE disks ALTER COLUMN seen_at TYPE TIMESTAMPTZ USING to_timestamp(seen_at / 1000.0);
ALTER TABLE health_samples ALTER COLUMN at TYPE TIMESTAMPTZ USING to_timestamp(at / 1000.0);
ALTER TABLE hosts ALTER COLUMN seen_at TYPE TIMESTAMPTZ USING to_timestamp(seen_at / 1000.0);
ALTER TABLE mount_samples ALTER COLUMN at TYPE TIMESTAMPTZ USING to_timestamp(at / 1000.0);
ALTER TABLE remediation_periods ALTER COLUMN created_at TYPE TIMESTAMPTZ USING to_timestamp(created_at / 1000.0);
ALTER TABLE remediation_periods ALTER COLUMN ended_at TYPE TIMESTAMPTZ USING to_timestamp(ended_at / 1000.0);
ALTER TABLE remediation_periods ALTER COLUMN started_at TYPE TIMESTAMPTZ USING to_timestamp(started_at / 1000.0);
ALTER TABLE remediation_records ALTER COLUMN created_at TYPE TIMESTAMPTZ USING to_timestamp(created_at / 1000.0);
ALTER TABLE remediation_records ALTER COLUMN ended_at TYPE TIMESTAMPTZ USING to_timestamp(ended_at / 1000.0);
ALTER TABLE restore_runs ALTER COLUMN created_at TYPE TIMESTAMPTZ USING to_timestamp(created_at / 1000.0);
ALTER TABLE restore_runs ALTER COLUMN ended_at TYPE TIMESTAMPTZ USING to_timestamp(ended_at / 1000.0);
ALTER TABLE restore_runs ALTER COLUMN started_at TYPE TIMESTAMPTZ USING to_timestamp(started_at / 1000.0);
ALTER TABLE servers ALTER COLUMN created_at TYPE TIMESTAMPTZ USING to_timestamp(created_at / 1000.0);
ALTER TABLE servers ALTER COLUMN last_checked_at TYPE TIMESTAMPTZ USING to_timestamp(last_checked_at / 1000.0);
ALTER TABLE servers ALTER COLUMN last_seen_at TYPE TIMESTAMPTZ USING to_timestamp(last_seen_at / 1000.0);
ALTER TABLE servers ALTER COLUMN updated_at TYPE TIMESTAMPTZ USING to_timestamp(updated_at / 1000.0);
ALTER TABLE sessions ALTER COLUMN created_at TYPE TIMESTAMPTZ USING to_timestamp(created_at / 1000.0);
ALTER TABLE sessions ALTER COLUMN expires_at TYPE TIMESTAMPTZ USING to_timestamp(expires_at / 1000.0);
ALTER TABLE storage_domains ALTER COLUMN seen_at TYPE TIMESTAMPTZ USING to_timestamp(seen_at / 1000.0);
ALTER TABLE storage_targets ALTER COLUMN created_at TYPE TIMESTAMPTZ USING to_timestamp(created_at / 1000.0);
ALTER TABLE storage_targets ALTER COLUMN last_check_at TYPE TIMESTAMPTZ USING to_timestamp(last_check_at / 1000.0);
ALTER TABLE storage_targets ALTER COLUMN updated_at TYPE TIMESTAMPTZ USING to_timestamp(updated_at / 1000.0);
ALTER TABLE users ALTER COLUMN created_at TYPE TIMESTAMPTZ USING to_timestamp(created_at / 1000.0);
ALTER TABLE users ALTER COLUMN last_login_at TYPE TIMESTAMPTZ USING to_timestamp(last_login_at / 1000.0);
ALTER TABLE users ALTER COLUMN updated_at TYPE TIMESTAMPTZ USING to_timestamp(updated_at / 1000.0);
ALTER TABLE verify_runs ALTER COLUMN created_at TYPE TIMESTAMPTZ USING to_timestamp(created_at / 1000.0);
ALTER TABLE verify_runs ALTER COLUMN ended_at TYPE TIMESTAMPTZ USING to_timestamp(ended_at / 1000.0);
ALTER TABLE verify_runs ALTER COLUMN started_at TYPE TIMESTAMPTZ USING to_timestamp(started_at / 1000.0);
ALTER TABLE vms ALTER COLUMN seen_at TYPE TIMESTAMPTZ USING to_timestamp(seen_at / 1000.0);
