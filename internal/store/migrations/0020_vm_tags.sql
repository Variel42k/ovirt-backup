-- Теги платформы обновляются из oVirt. Локальные теги хранит служба,
-- они сохраняются при синхронизации инвентаря, в том числе на KVM.
ALTER TABLE vms ADD COLUMN IF NOT EXISTS provider_tags JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE vms ADD COLUMN IF NOT EXISTS local_tags JSONB NOT NULL DEFAULT '[]'::jsonb;

