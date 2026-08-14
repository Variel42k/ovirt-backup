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

// CanWrite reports whether the role may change VM/backup state.
func (r Role) CanWrite() bool { return r == RoleAdmin || r == RoleOperator }

// CanAdmin reports whether the role may change connections, storages and users.
func (r Role) CanAdmin() bool { return r == RoleAdmin }

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
}

// Expired reports whether the session is no longer valid at t.
func (s *Session) Expired(t time.Time) bool { return t.After(s.ExpiresAt) }

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
