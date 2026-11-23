package model

import "time"

// IdentitySettings является переопределением базы данных для проверки подлинности браузера.
// Секреты принимаются на входе только на HTTP границе и шифруются Store.
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
	DefaultRole        string
	SubjectRoleMapping map[string]string
	AllowLocalLogin    bool
	SessionTTL         time.Duration
	RevalidateInterval time.Duration

	DomainName       string
	LDAPProviderName string
	LDAPURL          string
	LDAPUsersDN      string
	LDAPGroupsDN     string
	LDAPBindDN       string
	LDAPGroupMode    string
	DomainConnected  bool
	DomainCheckedAt  *time.Time
	UpdatedBy        string
	UpdatedAt        time.Time
}
