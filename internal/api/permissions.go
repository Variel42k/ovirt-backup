package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// roleCache держит настраиваемые роли, чтобы не читать их из базы на каждый
// запрос.
//
// Роли меняются раз в месяц, а проверяются несколько раз в секунду. Но кеш без
// срока — это отобранное право, которое продолжает действовать: администратор
// снял доступ, а служба о нём ещё не знает. Короткий срок делает задержку
// предсказуемой, а правка роли сбрасывает кеш сразу и не ждёт его.
type roleCache struct {
	mu       sync.RWMutex
	roles    map[model.Role]model.RoleDefinition
	loadedAt time.Time
}

const roleCacheTTL = 30 * time.Second

func newRoleCache() *roleCache { return &roleCache{} }

// invalidate сбрасывает кеш. Вызывается после любой правки роли.
func (c *roleCache) invalidate() {
	c.mu.Lock()
	c.roles, c.loadedAt = nil, time.Time{}
	c.mu.Unlock()
}

func (c *roleCache) get() (map[model.Role]model.RoleDefinition, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.roles == nil || time.Since(c.loadedAt) > roleCacheTTL {
		return nil, false
	}
	return c.roles, true
}

func (c *roleCache) put(roles map[model.Role]model.RoleDefinition) {
	c.mu.Lock()
	c.roles, c.loadedAt = roles, time.Now()
	c.mu.Unlock()
}

// resolveRole находит роль по имени: сначала среди встроенных, затем среди
// настраиваемых.
//
// Встроенные проверяются первыми и потому не могут быть переопределены записью
// в базе. Роль с именем admin, завёденная в обход, была бы отличным способом
// тихо расширить себе права.
func (s *Server) resolveRole(ctx context.Context, name model.Role) (model.RoleDefinition, bool) {
	for _, builtin := range model.BuiltinRoles() {
		if builtin.Name == name {
			return builtin, true
		}
	}

	roles, ok := s.roles.get()
	if !ok {
		loaded, err := s.store.ListRoles(ctx)
		if err != nil {
			// Отказ базы не должен превращаться в выдачу прав. Молча вернуть
			// «роли нет» — значит запретить всё, и это верное поведение: пустой
			// интерфейс заметят и разберутся, а лишний доступ — нет.
			s.log.Error().Err(err).Msg("не удалось прочитать роли")
			return model.RoleDefinition{}, false
		}
		roles = make(map[model.Role]model.RoleDefinition, len(loaded))
		for _, r := range loaded {
			roles[r.Name] = r
		}
		s.roles.put(roles)
	}

	role, ok := roles[name]
	return role, ok
}

// permissionsFor возвращает права роли.
func (s *Server) permissionsFor(ctx context.Context, name model.Role) []model.Permission {
	role, ok := s.resolveRole(ctx, name)
	if !ok {
		return nil
	}
	return role.Permissions
}

// requirePermission guards a handler with a single permission.
func (s *Server) requirePermission(perm model.Permission, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		if p == nil {
			writeJSON(w, http.StatusUnauthorized, errorResponse{
				Error: "требуется вход в систему", Code: "unauthorized",
			})
			return
		}
		if !p.Can(perm) {
			// Право называется в ответе намеренно. Скрывать его нечего — оно
			// есть в каталоге, — а разбор «почему у меня 403» без него
			// превращается в переписку с администратором.
			writeJSON(w, http.StatusForbidden, errorResponse{
				Error: "недостаточно прав для этой операции: требуется " + string(perm),
				Code:  "forbidden",
			})
			return
		}
		next(w, r)
	}
}

// perm — короткая запись для разметки маршрутов.
func (s *Server) perm(p model.Permission, next http.HandlerFunc) http.HandlerFunc {
	return s.requirePermission(p, next)
}
