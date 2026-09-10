package model

import "time"

// IdentitySettings is the database override for browser authentication.
// Secrets are write-only at the HTTP boundary and encrypted by Store.
type IdentitySettings struct {
	Enabled            bool
	Issuer             string
	BackchannelURL     string
	ClientID           string
	ClientSecret       string
	RedirectURL        string
	ButtonLabel        string
	GroupsClaim        string
	RoleMapping        map[string]string
	AllowLocalLogin    bool
	SessionTTL         time.Duration
	RevalidateInterval time.Duration

	DomainName       string
	LDAPProviderName string
	LDAPURL          string
	LDAPUsersDN      string
	LDAPGroupsDN     string
	LDAPBindDN       string
	DomainConnected  bool
	DomainCheckedAt  *time.Time
	UpdatedBy        string
	UpdatedAt        time.Time
}
