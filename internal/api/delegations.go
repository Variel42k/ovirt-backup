package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// Делегирование права голоса.
//
// Задача: согласующий уезжает, кворум перестаёт собираться, и работа встаёт до
// его возвращения. Резервная группа помогает лишь отчасти — она включается по
// таймауту, то есть после того, как заявка уже провисела положенное время.
//
// Устройство выбрано так, чтобы делегирование не стало обходным путём вокруг
// самого согласования:
//
//   - передаёт право только сам согласующий, за другого — нельзя;
//   - делегат назван поимённо и сначала входит под собой, поэтому в журнале
//     видно обоих;
//   - к токену нужен отдельный пароль, который передают другим каналом:
//     перехваченный токен сам по себе бесполезен;
//   - срок обязателен и ограничен сверху;
//   - голос засчитывается делегирующему, а не делегату, иначе один человек с
//     двумя делегированиями закрыл бы кворум в одиночку.
const (
	// Схема отличается от схемы токенов API намеренно: перепутать их местами
	// не должно быть возможно даже случайно. Токен согласования, вставленный в
	// заголовок Authorization, обязан просто не подойти.
	delegationScheme = "jhvd"
)

// generateDelegationToken выпускает токен делегирования.
//
// Целиком он существует только здесь и в единственном ответе на создание: в
// базу уходит хеш.
func generateDelegationToken() (token, prefix string, hash []byte, err error) {
	full, prefix, hash, err := generateAPIToken()
	if err != nil {
		return "", "", nil, err
	}
	// generateAPIToken выдаёт токен со своей схемой; здесь нужна другая.
	return delegationScheme + strings.TrimPrefix(full, apiTokenScheme), prefix, hash, nil
}

// splitDelegationToken разбирает предъявленный токен.
func splitDelegationToken(presented string) (prefix, secret string, ok bool) {
	parts := strings.Split(presented, apiTokenSeparator)
	if len(parts) != 3 || parts[0] != delegationScheme {
		return "", "", false
	}
	if parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// errDelegation — общий отказ для всех неудач проверки делегирования.
//
// Причина не называется намеренно. «Токен просрочен» и «неверный пароль» —
// это два разных ответа, из которых складывается перебор: первый подтверждает,
// что токен существует. Владельцу настоящего токена причина известна и так,
// он видит своё делегирование в списке.
var errDelegation = errors.New("делегирование не подтверждено")

// delegationCredentials — то, что предъявляет делегат.
type delegationCredentials struct {
	Token    string `json:"delegation_token,omitempty"`
	Password string `json:"delegation_password,omitempty"`
}

// present сообщает, предъявлено ли делегирование.
func (c delegationCredentials) present() bool {
	return strings.TrimSpace(c.Token) != "" || strings.TrimSpace(c.Password) != ""
}

// resolveDelegation проверяет предъявленное делегирование и возвращает того,
// чьим именем будет подан голос.
//
// caller — тот, кто вошёл в систему; group — группа, в которой идёт
// голосование.
func (s *Server) resolveDelegation(ctx context.Context, creds delegationCredentials,
	caller, group string) (*model.ApprovalDelegation, error) {

	prefix, secret, ok := splitDelegationToken(strings.TrimSpace(creds.Token))
	if !ok || creds.Password == "" {
		return nil, fmt.Errorf("%w: %w", errForbidden, errDelegation)
	}

	d, err := s.store.ApprovalDelegationByPrefix(ctx, prefix)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		// Пароль всё равно считается: без этого несуществующий префикс
		// отвечает мгновенно, а существующий — после bcrypt, и разница во
		// времени сама становится ответом.
		bcrypt.CompareHashAndPassword([]byte(dummyPasswordHash), []byte(creds.Password))
		return nil, fmt.Errorf("%w: %w", errForbidden, errDelegation)
	}

	// Сравнение за постоянное время: утечка через время ответа на токене —
	// показанная на практике атака, а не теоретическая.
	if subtle.ConstantTimeCompare(d.TokenHash, hashAPISecret(secret)) != 1 {
		bcrypt.CompareHashAndPassword([]byte(dummyPasswordHash), []byte(creds.Password))
		return nil, fmt.Errorf("%w: %w", errForbidden, errDelegation)
	}
	if err := bcrypt.CompareHashAndPassword(d.PasswordHash, []byte(creds.Password)); err != nil {
		return nil, fmt.Errorf("%w: %w", errForbidden, errDelegation)
	}
	if !d.Usable(time.Now().UTC()) {
		return nil, fmt.Errorf("%w: %w", errForbidden, errDelegation)
	}
	// Токен выдан конкретному человеку. Без этой проверки он работал бы у
	// любого, кому его переслали, — то есть был бы обычным общим паролем.
	if d.Delegate != caller {
		return nil, fmt.Errorf("%w: %w", errForbidden, errDelegation)
	}
	if !d.Covers(group) {
		return nil, fmt.Errorf("%w: делегирование не распространяется на группу %q",
			errForbidden, group)
	}

	return d, nil
}
