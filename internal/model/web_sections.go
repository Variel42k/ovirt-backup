package model

// WebSections управляет навигацией для каждой веб-сессии, независимо от RBAC.
// Он не останавливает фоновые задания и не отменяет разрешения API.
type WebSections struct {
	Discovery bool `json:"discovery"`
}

func DefaultWebSections() WebSections {
	return WebSections{Discovery: true}
}
