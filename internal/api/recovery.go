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

// RecoveryResult reports credentials invalidated by a host-side recovery.
type RecoveryResult struct {
	Sessions    int64
	APITokens   int64
	Delegations int64
}

// VerifyRecoveryToken proves that recovery was started by someone who can
// read the host-only token. Only its SHA-256 verifier is visible to the normal
// service process.
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

// RecoverLocalAccess resets one local password and, for an incident recovery,
// invalidates every credential that can be revoked in PostgreSQL.
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
