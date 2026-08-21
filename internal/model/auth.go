package model

import "time"

// Role controls what an authenticated principal may do.
type Role string

const (
	// RoleAdmin — полный доступ, включая управление серверами и пользователями.
	RoleAdmin Role = "admin"
	// RoleOperator — управление ВМ и бэкапами, без правки подключений и пользователей.
	RoleOperator Role = "operator"
	// RoleViewer — только чтение.
	RoleViewer Role = "viewer"
)

// Методов CanWrite и CanAdmin у роли больше нет: они сравнивали имя роли с
// «admin» и «operator», а имя настраиваемой роли произвольно. Что роль может —
// решает её набор прав, см. permission.go и role.go.

// Откуда взялась учётная запись.
const (
	// ProviderLocal — пароль хранится и проверяется здесь.
	ProviderLocal = "local"
	// ProviderOIDC — личность подтверждает внешний провайдер, пароля нет.
	ProviderOIDC = "oidc"
)

// User is an account, local or backed by an external identity provider.
type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	// PasswordHash пуст у внешних записей: пароля у них нет вовсе, и вход по
	// паролю для них невозможен.
	PasswordHash string `json:"-"`
	Role         Role   `json:"role"`
	Disabled     bool   `json:"disabled"`
	// Provider — ProviderLocal либо имя внешнего провайдера.
	Provider string `json:"provider"`
	// ExternalID — идентификатор у провайдера (sub). Связь ведётся по нему, а
	// не по имени: переименование в каталоге не должно заводить вторую запись
	// и терять вместе с ней права.
	ExternalID  string     `json:"external_id,omitempty"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// IsExternal reports whether the identity belongs to an external provider.
func (u *User) IsExternal() bool { return u.ExternalID != "" }

// Session is a server-side session referenced by an opaque cookie token.
type Session struct {
	Token     string    `json:"-"`
	UserID    string    `json:"user_id"`
	Username  string    `json:"username"`
	Role      Role      `json:"role"`
	UserAgent string    `json:"user_agent,omitempty"`
	RemoteIP  string    `json:"remote_ip,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
	// OIDCIDToken — токен личности, по которому эта сессия заведена. Нужен
	// провайдеру при выходе как id_token_hint. Пуст у входов по паролю и
	// наружу не отдаётся.
	OIDCIDToken string `json:"-"`
}

// Expired reports whether the session is no longer valid at t.
func (s *Session) Expired(t time.Time) bool { return t.After(s.ExpiresAt) }

// APIToken — доступ к API для того, кто не может держать куку: скрипта,
// системы мониторинга, соседней службы.
//
// У токена есть имя, роль и срок, потому что все три однажды понадобятся: имя
// — чтобы в журнале аудита было видно, кто именно это сделал; роль — чтобы
// выгрузка отчётов не могла удалить задание; срок — чтобы забытый токен
// переставал работать сам, а не ждал, пока о нём вспомнят.
type APIToken struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Prefix — открытая часть токена. По ней строка находится в таблице, и
	// только после этого сверяется секрет. По ней же токен узнаётся в журнале.
	Prefix     string     `json:"prefix"`
	Role       Role       `json:"role"`
	CreatedBy  string     `json:"created_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Disabled   bool       `json:"disabled"`

	// SecretHash не отдаётся наружу никогда: восстановить по нему токен нельзя,
	// но и показывать его незачем.
	SecretHash []byte `json:"-"`
}

// Expired reports whether the token is past its expiry at t.
func (t APIToken) Expired(at time.Time) bool {
	return t.ExpiresAt != nil && at.After(*t.ExpiresAt)
}

// Usable reports whether the token may authenticate a request at t.
func (t APIToken) Usable(at time.Time) bool {
	return !t.Disabled && !t.Expired(at)
}

// AuditEntry records a state-changing API call for traceability.
type AuditEntry struct {
	ID       int64     `json:"id"`
	Actor    string    `json:"actor"`
	Action   string    `json:"action"`
	Scope    Scope     `json:"scope"`
	ObjectID string    `json:"object_id,omitempty"`
	Detail   string    `json:"detail,omitempty"`
	Success  bool      `json:"success"`
	RemoteIP string    `json:"remote_ip,omitempty"`
	At       time.Time `json:"at"`
}
