package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/store"
)

const maxRecoveryTokenBytes = 4096

// RecoveryResult сообщает, что учётные данные аутентификации были аннулированы
// восстановлением на стороне узла.
type RecoveryResult struct {
	Sessions    int64
	APITokens   int64
	Delegations int64
}

// VerifyRecoveryToken доказывает, что восстановление было начато кем-то, кто может читать
// токен только для хоста. Для обычного процесса обслуживания виден только его SHA-256
// верификатор.
func VerifyRecoveryToken(expectedHash string, reader io.Reader) error {
	expectedHash = strings.TrimSpace(expectedHash)
	if expectedHash == "" {
		return fmt.Errorf("recovery-токен не настроен; повторно запустите доверенный установщик")
	}
	expected, err := hex.DecodeString(expectedHash)
	if err != nil || len(expected) != sha256.Size {
		return fmt.Errorf("auth.recovery_token_hash повреждён")
	}

	body, err := io.ReadAll(io.LimitReader(reader, maxRecoveryTokenBytes+1))
	if err != nil {
		return fmt.Errorf("чтение recovery-токена: %w", err)
	}
	if len(body) > maxRecoveryTokenBytes {
		return fmt.Errorf("recovery-токен слишком велик")
	}
	token := bytes.TrimSpace(body)
	if len(token) == 0 || bytes.ContainsAny(token, "\r\n") {
		return fmt.Errorf("recovery-токен должен содержать одну непустую строку")
	}
	actual := sha256.Sum256(token)
	if subtle.ConstantTimeCompare(actual[:], expected) != 1 {
		return fmt.Errorf("неверный recovery-токен")
	}
	return nil
}

// RecoverLocalAccess меняет локальный пароль. При восстановлении после инцидента также
// отзывает все учётные данные, которые можно отозвать в PostgreSQL.
func RecoverLocalAccess(ctx context.Context, st *store.Store, username, password string,
	revokeAll bool) (string, RecoveryResult, error) {
	password, revoked, err := resetPassword(ctx, st, username, password, revokeAll)
	if err != nil {
		return "", RecoveryResult{}, err
	}
	result := RecoveryResult{Sessions: revoked.Sessions, APITokens: revoked.APITokens,
		Delegations: revoked.Delegations}
	return password, result, nil
}
