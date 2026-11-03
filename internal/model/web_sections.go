package model

// WebSections controls navigation for every web session, independently of RBAC.
// It does not stop background jobs or revoke API permissions.
type WebSections struct {
	Discovery   bool `json:"discovery"`
	GitlabClean bool `json:"gitlab_clean"`
}

func DefaultWebSections() WebSections {
	return WebSections{Discovery: true, GitlabClean: true}
}
