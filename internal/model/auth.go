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

// User is a local account. External identity providers are out of scope; the
// service is meant to sit behind the operator's own perimeter.
type User struct {
	ID           string     `json:"id"`
	Username     string     `json:"username"`
	PasswordHash string     `json:"-"`
	Role         Role       `json:"role"`
	Disabled     bool       `json:"disabled"`
	LastLoginAt  *time.Time `json:"last_login_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

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
