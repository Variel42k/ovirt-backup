package webui

import "net/http"

// SetBrowserHeaders задаёт браузерные ограничения для приложения и API.
func SetBrowserHeaders(h http.Header, secure bool) {
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Permitted-Cross-Domain-Policies", "none")
	if secure {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
}
