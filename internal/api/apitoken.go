package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// Вид выданного токена: jhv.<префикс>.<секрет>.
//
// Разделённый вид нужен ради поиска. Хранить можно только хеш, а по хешу не
// поищешь: pgcrypto пришлось бы звать на каждую строку таблицы. Открытый
// префикс превращает поиск в обращение по индексу, и лишь найденная строка
// сверяется по секрету.
//
// Он же делает токен узнаваемым. Случайная строка в чужом скрипте не говорит
// ни о чём, а «jhv.» и восемь символов префикса находятся поиском и по журналу
// аудита, и по репозиторию, куда токен однажды попадёт по недосмотру.
const (
	apiTokenScheme = "jhv"
	// Разделитель — точка, и это не вкусовщина. Алфавит base64url — это
	// A–Z, a–z, 0–9, «-» и «_», то есть подчёркивание встречается и внутри
	// префикса, и внутри секрета. С ним в роли разделителя разбор ломался на
	// каждом шестидесятом с чем-то токене: выпускался он нормально, а
	// предъявить его было нельзя. Точки в base64url нет.
	apiTokenSeparator = "."
	// 6 байт — 8 символов base64url. Хватает, чтобы префиксы не сталкивались:
	// уникальность всё равно стережёт индекс, а выдача повторяется при
	// столкновении.
	apiTokenPrefixBytes = 6
	apiTokenSecretBytes = 32
)

// generateAPIToken выпускает токен и возвращает его целиком, его префикс и
// хеш секретной части.
//
// Целиком токен существует только здесь и в единственном ответе на запрос
// создания: в базу уходит хеш, и восстановить по нему выданное нельзя.
func generateAPIToken() (token, prefix string, hash []byte, err error) {
	prefixRaw := make([]byte, apiTokenPrefixBytes)
	if _, err = rand.Read(prefixRaw); err != nil {
		return "", "", nil, fmt.Errorf("нет источника случайных чисел: %w", err)
	}
	secretRaw := make([]byte, apiTokenSecretBytes)
	if _, err = rand.Read(secretRaw); err != nil {
		return "", "", nil, fmt.Errorf("нет источника случайных чисел: %w", err)
	}

	prefix = base64.RawURLEncoding.EncodeToString(prefixRaw)
	secret := base64.RawURLEncoding.EncodeToString(secretRaw)
	sum := hashAPISecret(secret)

	token = apiTokenScheme + apiTokenSeparator + prefix + apiTokenSeparator + secret
	return token, prefix, sum, nil
}

// hashAPISecret считает то, что хранится вместо токена.
//
// SHA-256, а не bcrypt, которым хешируются пароли. Разница не в небрежности, а
// в том, что защищают. Пароль придумывает человек, вариантов у него мало, и
// медленный хеш — единственное, что мешает перебрать их все. В секрете здесь
// 256 бит из crypto/rand: перебирать нечего, и никакая медленность этого не
// улучшит. Зато bcrypt считался бы на каждом обращении к API — сотня
// миллисекунд на запрос, который сам по себе занимает единицы.
func hashAPISecret(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// splitAPIToken разбирает предъявленный токен на префикс и секрет.
func splitAPIToken(presented string) (prefix, secret string, ok bool) {
	parts := strings.Split(presented, apiTokenSeparator)
	if len(parts) != 3 || parts[0] != apiTokenScheme {
		return "", "", false
	}
	if parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// principalFromAPIToken проверяет токен из базы.
//
// Возвращает nil, когда токена нет, он отозван, просрочен или секрет не сошёлся.
// Различать эти случаи в ответе нельзя: любой такой ответ сообщал бы владельцу
// чужого токена, существует ли он.
func (s *Server) principalFromAPIToken(ctx context.Context, presented string) *principal {
	prefix, secret, ok := splitAPIToken(presented)
	if !ok {
		return nil
	}

	token, err := s.store.GetAPITokenByPrefix(ctx, prefix)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Warn().Err(err).Msg("не удалось проверить токен доступа")
		}
		return nil
	}

	// Сравнение за постоянное время: утечка через время ответа на токене —
	// не теоретическая, а показанная на практике атака.
	if subtle.ConstantTimeCompare(token.SecretHash, hashAPISecret(secret)) != 1 {
		return nil
	}
	now := time.Now().UTC()
	if !token.Usable(now) {
		return nil
	}

	// Отметка об использовании — не чаще раза в минуту. Писать на каждый
	// запрос значило бы превратить проверку токена в запись в базу, а
	// опрашивающий монитор ходит сюда несколько раз в секунду.
	if token.LastUsedAt == nil || now.Sub(*token.LastUsedAt) > time.Minute {
		s.store.TouchAPIToken(context.WithoutCancel(ctx), token.ID)
	}

	return &principal{
		Username:    apiTokenActor(token.Name),
		Role:        token.Role,
		Permissions: s.permissionsFor(ctx, token.Role),
	}
}

// apiTokenActor — как токен называется в журнале аудита.
//
// Префикс «токен:» отделяет его от учётной записи человека: имя у токена
// произвольное, и без пометки строка аудита читалась бы как действие
// пользователя с таким именем.
func apiTokenActor(name string) string { return "токен:" + name }

// validateTokenRole проверяет, что роль существует.
//
// Незнакомая роль не ошибка настройки, а выданный доступ, который ничего не
// может: проверки прав сравнивают со списком, и роль вне списка не проходит ни
// одну из них. Токен при этом выглядит рабочим.
func validateTokenRole(role model.Role) error {
	switch role {
	case model.RoleAdmin, model.RoleOperator, model.RoleViewer:
		return nil
	default:
		return fmt.Errorf("неизвестная роль %q: допустимы admin, operator, viewer", role)
	}
}
