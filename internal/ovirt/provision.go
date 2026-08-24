package ovirt

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Настройка отдельной учётной записи для службы.
//
// Смысл в том, чтобы административные учётные данные движка вводились один раз
// и здесь же забывались. Служба под ними заводит роль с минимальным набором
// прав, выдаёт её сервисной записи и дальше работает только от её имени.
//
// Пароль администратора при этом нигде не сохраняется. Это и есть вся разница:
// хранимая административная учётка означает, что любая дыра в этой службе —
// это дыра во всей виртуализации, потому что учётные данные лежат в её базе и
// годятся для прямых запросов к движку в обход любых проверок.
//
// Чего здесь нет и быть не может: создания самой учётной записи. Движок
// пользователями не управляет — они приходят из домена аутентификации, а во
// встроенном домене заводятся утилитой ovirt-aaa-jdbc-tool на самом движке.
// Через REST можно только показать движку уже существующего пользователя
// домена и выдать ему права, что здесь и делается.

// ErrObjectNotFound — среди перечисленного движком нужного объекта нет.
//
// Отличается от IsNotFound: тот про ответ 404 на конкретный адрес, а этот про
// поиск по списку, который движок отдал успешно.
var ErrObjectNotFound = errors.New("объект не найден на движке")

// Role — роль движка.
type Role struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	Administrative bool   `json:"administrative"`
}

type roleList struct {
	Role []Role `json:"role"`
}

// Permit — одно право внутри роли.
type Permit struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type permitList struct {
	Permit []Permit `json:"permit"`
}

// User — запись пользователя, известная движку.
type User struct {
	ID        string `json:"id"`
	UserName  string `json:"user_name"`
	Principal string `json:"principal,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Domain    Ref    `json:"domain"`
}

type userList struct {
	User []User `json:"user"`
}

// Domain — домен аутентификации движка.
type Domain struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type domainList struct {
	Domain []Domain `json:"domain"`
}

type permissionRequest struct {
	Role Ref `json:"role"`
	User Ref `json:"user"`
}

// ListRoles возвращает роли движка.
func (c *Client) ListRoles(ctx context.Context) ([]Role, error) {
	var out roleList
	if err := c.get(ctx, "/roles", &out); err != nil {
		return nil, fmt.Errorf("список ролей: %w", err)
	}
	return out.Role, nil
}

// RoleByName ищет роль по имени.
func (c *Client) RoleByName(ctx context.Context, name string) (*Role, error) {
	roles, err := c.ListRoles(ctx)
	if err != nil {
		return nil, err
	}
	for i := range roles {
		if strings.EqualFold(roles[i].Name, name) {
			return &roles[i], nil
		}
	}
	return nil, ErrObjectNotFound
}

// CreateRole заводит роль с указанными правами.
//
// Роль административная: без этого признака движок не отдаёт её обладателю
// доступ к API администрирования, а бэкап читает инвентарь целиком, а не
// только «свои» объекты.
func (c *Client) CreateRole(ctx context.Context, name, description string, permits []string) (*Role, error) {
	body := map[string]any{
		"name":           name,
		"description":    description,
		"administrative": true,
	}
	var created Role
	if err := c.post(ctx, "/roles", body, &created); err != nil {
		return nil, fmt.Errorf("создание роли %q: %w", name, err)
	}

	for _, permit := range permits {
		if err := c.AddRolePermit(ctx, created.ID, permit); err != nil {
			return nil, err
		}
	}
	return &created, nil
}

// AddRolePermit добавляет право в роль.
//
// Уже имеющееся право движок отвергает конфликтом — это не ошибка настройки, а
// признак того, что роль уже настроена, и повторный запуск не должен из-за
// этого падать.
func (c *Client) AddRolePermit(ctx context.Context, roleID, permit string) error {
	err := c.post(ctx, "/roles/"+roleID+"/permits", Permit{Name: permit}, nil)
	if err == nil || IsConflict(err) {
		return nil
	}
	return fmt.Errorf("право %q для роли: %w", permit, err)
}

// RolePermits возвращает права роли.
func (c *Client) RolePermits(ctx context.Context, roleID string) ([]Permit, error) {
	var out permitList
	if err := c.get(ctx, "/roles/"+roleID+"/permits", &out); err != nil {
		return nil, fmt.Errorf("права роли: %w", err)
	}
	return out.Permit, nil
}

// ListDomains возвращает домены аутентификации движка.
func (c *Client) ListDomains(ctx context.Context) ([]Domain, error) {
	var out domainList
	if err := c.get(ctx, "/domains", &out); err != nil {
		return nil, fmt.Errorf("список доменов: %w", err)
	}
	return out.Domain, nil
}

// UserByName ищет пользователя среди уже известных движку.
func (c *Client) UserByName(ctx context.Context, name string) (*User, error) {
	users, err := c.ListUsersRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("список пользователей: %w", err)
	}
	for i := range users {
		if strings.EqualFold(users[i].UserName, name) ||
			strings.EqualFold(users[i].Principal, name) {
			return &users[i], nil
		}
	}
	return nil, ErrObjectNotFound
}

// AddDirectoryUser показывает движку пользователя домена.
//
// Создать учётную запись это не может: движок лишь запоминает того, кто уже
// есть в каталоге. Если пользователя в домене нет, движок ответит отказом, и
// заводить его нужно средствами самого каталога.
func (c *Client) AddDirectoryUser(ctx context.Context, principal, domainName string) (*User, error) {
	body := map[string]any{
		"user_name": principal,
		"principal": principal,
		"domain":    map[string]any{"name": domainName},
	}
	var created User
	if err := c.post(ctx, "/users", body, &created); err != nil {
		return nil, fmt.Errorf("добавление пользователя %q из домена %q: %w",
			principal, domainName, err)
	}
	return &created, nil
}

// DefaultBackupPermits — права, которые служба просит для своей роли.
//
// Набор предварительный и это важно понимать. Имена групп действий движок
// проверяет сам: неизвестное он отвергнет, и тогда в ошибке будет видно, какое
// именно. Полнота набора проверяется не чтением документации, а вызовом
// CheckAccess после настройки — он показывает, что под этой ролью реально
// получается, и не даёт принять «роль создалась» за «бэкап заработает».
//
// Набор можно передать в запросе на подключение полем permits: у разных версий
// движка состав групп действий отличается, и подставить свой список должно быть
// можно, не пересобирая службу.
var DefaultBackupPermits = []string{
	"login",
	"backup_disk",
	"create_disk",
	"delete_disk",
	"configure_disk_storage",
	"access_image_transfer",
	"manipulate_vm_snapshots",
	"create_vm",
	"edit_vm_properties",
	"configure_vm_storage",
}

// AccessCheck — результат одной проверки доступа.
type AccessCheck struct {
	// What — что проверялось, словами оператора.
	What string `json:"what"`
	OK   bool   `json:"ok"`
	// Error — почему не получилось. Пусто при успехе.
	Error string `json:"error,omitempty"`
	// Required — без этого бэкап не работает вовсе.
	Required bool `json:"required"`
}

// AccessReport — что доступно под текущими учётными данными.
type AccessReport struct {
	Checks []AccessCheck `json:"checks"`
	// Ready — все обязательные проверки прошли.
	Ready bool `json:"ready"`
}

// CheckAccess выясняет, что движок разрешает этой учётной записи.
//
// Проверка идёт только чтением. Убедиться, что бэкап пройдёт целиком, можно
// было бы лишь запустив его, — а запускать бэкап чужой машины ради проверки
// прав нельзя: это нагрузка на гипервизор и снимок, которого никто не просил.
//
// Поэтому проверяется то, без чего бэкап не начнётся в принципе: вход, чтение
// инвентаря и доступ к перечню дисков. Недостающее право на сам съём проявится
// первым же запуском задания и будет видно в его ошибке.
func (c *Client) CheckAccess(ctx context.Context) AccessReport {
	report := AccessReport{Ready: true}

	add := func(what string, required bool, err error) {
		check := AccessCheck{What: what, OK: err == nil, Required: required}
		if err != nil {
			check.Error = err.Error()
			if required {
				report.Ready = false
			}
		}
		report.Checks = append(report.Checks, check)
	}

	_, err := c.Info(ctx)
	add("вход в API движка", true, err)
	if err != nil {
		// Дальше проверять нечего: без входа всё остальное ответит тем же.
		return report
	}

	// serverID здесь пуст: он попадает только в возвращаемые объекты как
	// пометка, откуда они, а нам важен сам факт доступа.
	_, err = c.ListClusters(ctx, "")
	add("чтение списка кластеров", true, err)

	_, err = c.ListHosts(ctx, "")
	add("чтение списка хостов", false, err)

	vms, _, err := c.listVMsWithAttachments(ctx, "")
	add("чтение списка виртуальных машин", true, err)

	// Диски проверяются у первой же машины: право на инвентарь и право на её
	// диски в движке разные, и роль, дающая только первое, выглядит рабочей до
	// самого запуска бэкапа.
	if err == nil && len(vms) > 0 {
		_, diskErr := c.ListVMDisks(ctx, vms[0].ID)
		add("чтение дисков виртуальной машины", true, diskErr)
	}

	_, err = c.ListStorageDomains(ctx, "")
	add("чтение доменов хранения", true, err)

	return report
}

// ExcessPrivilege — возможность, которой у службы быть не должно.
type ExcessPrivilege struct {
	What string `json:"what"`
	Why  string `json:"why"`
}

// CheckExcessPrivileges выясняет, может ли учётная запись больше, чем нужно
// для резервного копирования.
//
// Определяется фактом, а не по имени. Догадка «admin@internal — значит
// администратор» ошибается в обе стороны: административную запись называют как
// угодно, а безобидное на вид имя может нести роль SuperUser. Здесь просто
// пробуется то, что службе не нужно никогда, и если получается — значит прав
// больше необходимого.
func (c *Client) CheckExcessPrivileges(ctx context.Context) []ExcessPrivilege {
	var found []ExcessPrivilege

	if _, err := c.ListUsersRaw(ctx); err == nil {
		found = append(found, ExcessPrivilege{
			What: "управление пользователями движка",
			Why: "учётная запись видит список пользователей — значит может и раздавать " +
				"права. Резервному копированию это не нужно",
		})
	}
	if _, err := c.ListRoles(ctx); err == nil {
		found = append(found, ExcessPrivilege{
			What: "управление ролями движка",
			Why: "учётная запись читает роли — обычно это признак административной " +
				"роли уровня системы",
		})
	}
	return found
}

// ListUsersRaw возвращает пользователей, известных движку.
//
// Отдельно от UserByName: там отсутствие доступа — просто «не нашли», а здесь
// важна именно ошибка, по ней и определяется уровень прав.
func (c *Client) ListUsersRaw(ctx context.Context) ([]User, error) {
	var out userList
	if err := c.get(ctx, "/users", &out); err != nil {
		return nil, err
	}
	return out.User, nil
}

// GrantSystemPermission выдаёт роль пользователю на уровне всей системы.
//
// Уровень системы, а не отдельного кластера: бэкап должен видеть весь
// инвентарь, а восстановление — создавать ВМ там, куда укажет оператор.
func (c *Client) GrantSystemPermission(ctx context.Context, userID, roleID string) error {
	body := permissionRequest{Role: Ref{ID: roleID}, User: Ref{ID: userID}}
	err := c.post(ctx, "/permissions", body, nil)
	if err == nil || IsConflict(err) {
		return nil
	}
	return fmt.Errorf("назначение роли пользователю: %w", err)
}
