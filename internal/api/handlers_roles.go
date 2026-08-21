package api

import (
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// roleNamePattern ограничивает имя роли тем, что безопасно подставлять в URL,
// в сопоставление групп провайдера и в чужие скрипты.
var roleNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,30}$`)

type rolePayload struct {
	Name        string             `json:"name"`
	Title       string             `json:"title"`
	Description string             `json:"description"`
	Permissions []model.Permission `json:"permissions"`
}

// handlePermissionCatalog отдаёт разделы и права для редактора ролей.
//
// Каталог приходит с сервера, а не повторяется в интерфейсе: два списка прав
// разошлись бы, и редактор предлагал бы выдавать право, которого нет, либо
// умалчивал о существующем.
func (s *Server) handlePermissionCatalog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sections": model.PermissionCatalog()})
}

func (s *Server) handleListRoles(w http.ResponseWriter, r *http.Request) {
	roles := model.BuiltinRoles()

	custom, err := s.store.ListRoles(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	roles = append(roles, custom...)
	writeList(w, roles)
}

func (s *Server) handleCreateRole(w http.ResponseWriter, r *http.Request) {
	var payload rolePayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}

	payload.Name = strings.ToLower(strings.TrimSpace(payload.Name))
	if !roleNamePattern.MatchString(payload.Name) {
		s.writeError(w, r, badRequest(
			"имя роли: строчные латинские буквы, цифры, дефис и подчёркивание, "+
				"от двух до тридцати одного символа, начинается с буквы"))
		return
	}
	if model.IsBuiltinRole(model.Role(payload.Name)) {
		s.writeError(w, r, badRequest(
			"роль %q встроенная: выберите другое имя", payload.Name))
		return
	}
	if err := s.validateRolePayload(&payload); err != nil {
		s.writeError(w, r, err)
		return
	}

	role := &model.RoleDefinition{
		Name: model.Role(payload.Name), Title: payload.Title,
		Description: payload.Description, Permissions: payload.Permissions,
	}
	if err := s.store.CreateRole(r.Context(), role); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.roles.invalidate()
	s.audit(r, "role.create", model.ScopeServer, role.ID, true, describeRole(role))
	writeJSON(w, http.StatusCreated, role)
}

func (s *Server) handleUpdateRole(w http.ResponseWriter, r *http.Request) {
	role, err := s.store.GetRole(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	var payload rolePayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.validateRolePayload(&payload); err != nil {
		s.writeError(w, r, err)
		return
	}

	// Имя не меняется: на него ссылаются учётные записи, сопоставление групп у
	// провайдера и выданные токены. Переименование оставило бы их указывать на
	// роль, которой больше нет, и права пропали бы без всякого сообщения.
	role.Title = payload.Title
	role.Description = payload.Description
	role.Permissions = payload.Permissions

	if err := s.store.UpdateRole(r.Context(), role); err != nil {
		s.writeError(w, r, err)
		return
	}
	// Сброс кеша, а не ожидание его срока: правка роли — это чаще всего
	// отобранное право, и действовать оно должно сразу.
	s.roles.invalidate()
	s.audit(r, "role.update", model.ScopeServer, role.ID, true, describeRole(role))
	writeJSON(w, http.StatusOK, role)
}

func (s *Server) handleDeleteRole(w http.ResponseWriter, r *http.Request) {
	role, err := s.store.GetRole(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	// Учётная запись со ссылкой на удалённую роль не получает прав вовсе, и
	// человек узнаёт об этом, войдя в пустой интерфейс. Лучше отказать здесь.
	used, err := s.store.CountUsersWithRole(r.Context(), role.Name)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if used > 0 {
		s.writeError(w, r, badRequest(
			"роль %q назначена %d учётным записям: переведите их на другую роль",
			role.Name, used))
		return
	}

	if err := s.store.DeleteRole(r.Context(), role.ID); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.roles.invalidate()
	s.audit(r, "role.delete", model.ScopeServer, role.ID, true, string(role.Name))
	w.WriteHeader(http.StatusNoContent)
}

// validateRolePayload проверяет название и состав прав.
func (s *Server) validateRolePayload(payload *rolePayload) error {
	payload.Title = strings.TrimSpace(payload.Title)
	if payload.Title == "" {
		return badRequest("нужно название роли: его видно в списке пользователей")
	}

	// Повторы убираются здесь, а не при проверке прав: одно и то же право,
	// записанное дважды, ничего не меняет в доступе, но в редакторе выглядит
	// как две разные галочки.
	seen := map[model.Permission]bool{}
	unique := make([]model.Permission, 0, len(payload.Permissions))
	for _, p := range payload.Permissions {
		if !model.ValidPermission(p) {
			return badRequest("неизвестное право %q", p)
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		unique = append(unique, p)
	}
	if len(unique) == 0 {
		return badRequest("роль без прав ничего не открывает: выберите хотя бы одно")
	}

	// Порядок каталога, а не порядок присланного: так две одинаковые по сути
	// роли выглядят одинаково и в ответе API, и в журнале аудита.
	catalog := model.AllPermissions()
	slices.SortFunc(unique, func(a, b model.Permission) int {
		return slices.Index(catalog, a) - slices.Index(catalog, b)
	})
	payload.Permissions = unique
	return nil
}

// describeRole — строка для журнала аудита.
func describeRole(r *model.RoleDefinition) string {
	parts := make([]string, 0, len(r.Permissions))
	for _, p := range r.Permissions {
		parts = append(parts, string(p))
	}
	return string(r.Name) + ": " + strings.Join(parts, " ")
}
