package api

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// securityHeaders применяется защита браузера как к API, так и к SPA ответам.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Permitted-Cross-Domain-Policies", "none")
		if s.secureCookies() {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// csrf отклоняет изменяющие состояние запросы браузера, которые несут сеансовый файл
// cookie, но не исходят от этой установки (или явного CORS источника).
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || csrfSafeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}

		origin := strings.TrimSpace(r.Header.Get("Origin"))
		_, cookieErr := r.Cookie(sessionCookie)
		hasSession := cookieErr == nil
		hasBearer := strings.HasPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "Bearer ")
		if origin == "" {
			// Клиенты командной строки не отправляют метаданные Origin или Fetch. Браузеры
			// делают это, поэтому явно созданный браузером запрос cookie должен иметь
			// проверяемый Origin, в то время как администрирование на основе curl
			// продолжает работать.
			if hasSession && !hasBearer && r.Header.Get("Sec-Fetch-Site") != "" {
				writeJSON(w, http.StatusForbidden, errorResponse{
					Error: "запрос с cookie отклонён: отсутствует Origin", Code: "csrf_origin_required",
				})
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if !s.allowedBrowserOrigin(origin, r) {
			writeJSON(w, http.StatusForbidden, errorResponse{
				Error: "источник запроса не разрешён", Code: "csrf_origin_denied",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func csrfSafeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

func (s *Server) allowedBrowserOrigin(origin string, r *http.Request) bool {
	if slices.Contains(s.cfg.Server.CORSOrigins, origin) {
		return true
	}
	presented, err := url.Parse(origin)
	if err != nil || (presented.Scheme != "http" && presented.Scheme != "https") || presented.Host == "" {
		return false
	}

	if external := strings.TrimSpace(s.cfg.Server.ExternalURL); external != "" {
		expected, err := url.Parse(external)
		return err == nil && strings.EqualFold(presented.Scheme, expected.Scheme) &&
			strings.EqualFold(presented.Host, expected.Host)
	}

	scheme := "http"
	if r.TLS != nil || s.cfg.Server.TLS.Enabled {
		scheme = "https"
	}
	return strings.EqualFold(presented.Scheme, scheme) && strings.EqualFold(presented.Host, r.Host)
}
