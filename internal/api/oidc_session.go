package api

import (
	"context"
	"errors"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/store"
	"golang.org/x/oauth2"
)

type oidcSessionData struct{ refresh, subject, issuer string }

func oidcRevalidateInterval(cfg config.OIDCConfig) time.Duration {
	if cfg.RevalidateInterval > 0 {
		return cfg.RevalidateInterval
	}
	return 5 * time.Minute
}

func (s *Server) revalidateOIDCSession(ctx context.Context, session *model.Session) (*model.Session, error) {
	if session.OIDCIDToken == "" {
		return session, nil
	}
	client, oidcCfg := s.oidcSnapshot()
	if client == nil || session.OIDCIssuer != oidcCfg.Issuer {
		_ = s.store.DeleteSession(ctx, session.Token)
		return nil, store.ErrNotFound
	}
	if time.Since(session.OIDCCheckedAt) < oidcRevalidateInterval(oidcCfg) {
		return session, nil
	}
	result, err, _ := s.oidcRefresh.Do(session.Token, func() (any, error) {
		// Общая операция имеет свой ограниченный срок службы: отключенный браузер не должен
		// отменять вращение токена под другими запросами для этой сессии.
		checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), oidcExchangeTimeout)
		defer cancel()
		fresh, err := s.store.GetSession(checkCtx, session.Token)
		if err != nil {
			return nil, err
		}
		if time.Since(fresh.OIDCCheckedAt) < oidcRevalidateInterval(oidcCfg) {
			return fresh, nil
		}
		reject := func() (any, error) {
			_ = s.store.DeleteSession(checkCtx, fresh.Token)
			return nil, store.ErrNotFound
		}
		if fresh.OIDCRefreshToken == "" || fresh.OIDCSubject == "" {
			return reject()
		}
		cfg, verifier, err := client.connect(checkCtx)
		if err != nil {
			return nil, err
		}
		checkCtx = client.requestContext(checkCtx)
		token, err := cfg.TokenSource(checkCtx, &oauth2.Token{RefreshToken: fresh.OIDCRefreshToken}).Token()
		if err != nil {
			var invalid *oauth2.RetrieveError
			if errors.As(err, &invalid) && invalid.ErrorCode == "invalid_grant" {
				return reject()
			}
			return nil, errors.New("провайдер временно недоступен для проверки сессии")
		}
		// Обновленный маркер идентификатора проверяется при поставке. Предпочитают его
		// groups потому, что некоторые поставщики умышленно упускают их из UserInfo; Когда
		// они отсутствуют, UserInfo все еще принимается только по одному и тому же
		// предмету.
		var groups []string
		if raw, ok := token.Extra("id_token").(string); ok && raw != "" {
			id, verifyErr := verifier.Verify(checkCtx, raw)
			if verifyErr != nil || id.Subject != fresh.OIDCSubject {
				return reject()
			}
			var claims map[string]any
			if claimsErr := id.Claims(&claims); claimsErr != nil {
				return reject()
			}
			groups = claimStrings(lookupClaim(claims, oidcCfg.GroupsClaim))
		}
		if len(groups) == 0 {
			groups, err = client.groupsFromUserInfo(checkCtx, token, fresh.OIDCSubject)
			if err != nil {
				return reject()
			}
		}
		role, err := mapOIDCSubjectRole(oidcCfg, fresh.OIDCSubject, groups)
		if err != nil {
			return reject()
		}
		if token.RefreshToken == "" {
			token.RefreshToken = fresh.OIDCRefreshToken
		}
		if err := s.store.RevalidateOIDCSession(checkCtx, fresh, role, token.RefreshToken); err != nil {
			return reject()
		}
		return s.store.GetSession(checkCtx, fresh.Token)
	})
	if err != nil {
		return nil, err
	}
	return result.(*model.Session), nil
}
