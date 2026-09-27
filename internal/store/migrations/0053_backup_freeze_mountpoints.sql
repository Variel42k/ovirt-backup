-- Какие файловые системы гостя замораживать; пусто — все, как раньше.
--
-- Только KVM: libvirt передаёт список агенту (guest-fsfreeze-freeze-list).
-- Нужен узлам Kubernetes, где заморозка всех ФС останавливает запись etcd:
-- замораживается только том СУБД.
ALTER TABLE backup_jobs
    ADD COLUMN IF NOT EXISTS freeze_mountpoints JSONB NOT NULL DEFAULT '[]'::jsonb;
