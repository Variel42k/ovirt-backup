package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// IdentitySettings returns the optional database override. found=false means
// the YAML/environment configuration remains authoritative.
func (s *Store) IdentitySettings(ctx context.Context) (out model.IdentitySettings, found bool, err error) {
	var (
		secretEnc, mapping, subjects  string
		checked                       sql.NullTime
		ttlSeconds, revalidateSeconds int64
	)
	err = s.db.QueryRow(ctx, `SELECT enabled, issuer, backchannel_url, client_id,
		client_secret_enc, redirect_url, button_label, groups_claim, role_mapping,
		allow_local_login, session_ttl_seconds, revalidate_interval_seconds,
		domain_name, ldap_provider_name, ldap_url, ldap_users_dn, ldap_groups_dn,
		ldap_bind_dn, domain_connected, domain_checked_at, updated_by, updated_at,
		default_role, subject_role_mapping, ldap_group_mode
		FROM identity_settings WHERE id=1`).Scan(
		&out.Enabled, &out.Issuer, &out.BackchannelURL, &out.ClientID,
		&secretEnc, &out.RedirectURL, &out.ButtonLabel, &out.GroupsClaim, &mapping,
		&out.AllowLocalLogin, &ttlSeconds, &revalidateSeconds,
		&out.DomainName, &out.LDAPProviderName, &out.LDAPURL, &out.LDAPUsersDN,
		&out.LDAPGroupsDN, &out.LDAPBindDN, &out.DomainConnected, &checked,
		&out.UpdatedBy, &out.UpdatedAt, &out.DefaultRole, &subjects, &out.LDAPGroupMode)
	if errors.Is(err, sql.ErrNoRows) {
		return model.IdentitySettings{}, false, nil
	}
	if err != nil {
		return model.IdentitySettings{}, false, fmt.Errorf("read identity settings: %w", err)
	}
	out.ClientSecret, err = s.cipher.Decrypt(secretEnc)
	if err != nil {
		return model.IdentitySettings{}, false, fmt.Errorf("decrypt OIDC client secret: %w", err)
	}
	out.RoleMapping = map[string]string{}
	decodeJSON(mapping, &out.RoleMapping)
	out.SubjectRoleMapping = map[string]string{}
	decodeJSON(subjects, &out.SubjectRoleMapping)
	out.SessionTTL = time.Duration(ttlSeconds) * time.Second
	out.RevalidateInterval = time.Duration(revalidateSeconds) * time.Second
	out.DomainCheckedAt = nullTime(checked)
	out.UpdatedAt = utc(out.UpdatedAt)
	return out, true, nil
}

// SetIdentitySettings persists the complete OIDC configuration. Keycloak
// administration credentials and the LDAP bind password never reach this
// method and therefore cannot accidentally become durable application data.
func (s *Store) SetIdentitySettings(ctx context.Context, value model.IdentitySettings) error {
	secretEnc, err := s.cipher.Encrypt(value.ClientSecret)
	if err != nil {
		return fmt.Errorf("encrypt OIDC client secret: %w", err)
	}
	mapping := encodeJSON(value.RoleMapping)
	now := time.Now().UTC()
	_, err = s.db.Exec(ctx, `INSERT INTO identity_settings (
		id, enabled, issuer, backchannel_url, client_id, client_secret_enc,
		redirect_url, button_label, groups_claim, role_mapping, allow_local_login,
		session_ttl_seconds, revalidate_interval_seconds, domain_name,
		ldap_provider_name, ldap_url, ldap_users_dn, ldap_groups_dn, ldap_bind_dn,
		domain_connected, domain_checked_at, updated_by, updated_at,
		default_role, subject_role_mapping, ldap_group_mode)
		VALUES (1,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (id) DO UPDATE SET
		enabled=EXCLUDED.enabled, issuer=EXCLUDED.issuer,
		backchannel_url=EXCLUDED.backchannel_url, client_id=EXCLUDED.client_id,
		client_secret_enc=EXCLUDED.client_secret_enc, redirect_url=EXCLUDED.redirect_url,
		button_label=EXCLUDED.button_label, groups_claim=EXCLUDED.groups_claim,
		role_mapping=EXCLUDED.role_mapping, allow_local_login=EXCLUDED.allow_local_login,
		session_ttl_seconds=EXCLUDED.session_ttl_seconds,
		revalidate_interval_seconds=EXCLUDED.revalidate_interval_seconds,
		domain_name=EXCLUDED.domain_name, ldap_provider_name=EXCLUDED.ldap_provider_name,
		ldap_url=EXCLUDED.ldap_url, ldap_users_dn=EXCLUDED.ldap_users_dn,
		ldap_groups_dn=EXCLUDED.ldap_groups_dn, ldap_bind_dn=EXCLUDED.ldap_bind_dn,
		domain_connected=EXCLUDED.domain_connected,
		domain_checked_at=EXCLUDED.domain_checked_at,
		updated_by=EXCLUDED.updated_by, updated_at=EXCLUDED.updated_at,
		default_role=EXCLUDED.default_role, subject_role_mapping=EXCLUDED.subject_role_mapping,
		ldap_group_mode=EXCLUDED.ldap_group_mode`,
		value.Enabled, value.Issuer, value.BackchannelURL, value.ClientID, secretEnc,
		value.RedirectURL, value.ButtonLabel, value.GroupsClaim, mapping,
		value.AllowLocalLogin, int64(value.SessionTTL/time.Second),
		int64(value.RevalidateInterval/time.Second), value.DomainName,
		value.LDAPProviderName, value.LDAPURL, value.LDAPUsersDN,
		value.LDAPGroupsDN, value.LDAPBindDN, value.DomainConnected,
		value.DomainCheckedAt, value.UpdatedBy, now, value.DefaultRole,
		encodeJSON(value.SubjectRoleMapping), value.LDAPGroupMode)
	if err != nil {
		return fmt.Errorf("save identity settings: %w", err)
	}
	value.UpdatedAt = now
	return nil
}
