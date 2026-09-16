ALTER TABLE identity_settings ADD COLUMN default_role TEXT NOT NULL DEFAULT '';
ALTER TABLE identity_settings ADD COLUMN subject_role_mapping JSONB NOT NULL DEFAULT '{}';
ALTER TABLE identity_settings ADD COLUMN ldap_group_mode TEXT NOT NULL DEFAULT 'read-only';
