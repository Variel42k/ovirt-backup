-- Provider tags are refreshed from oVirt; local tags are owned by this
-- service and survive every inventory synchronization (notably for KVM).
ALTER TABLE vms ADD COLUMN IF NOT EXISTS provider_tags JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE vms ADD COLUMN IF NOT EXISTS local_tags JSONB NOT NULL DEFAULT '[]'::jsonb;

