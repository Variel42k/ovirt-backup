package api

import (
	"context"
	"regexp"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

var storageDomainIdentifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)
var storageDomainNamespace = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

func (s *Server) validateStorageDomain(ctx context.Context, target *model.StorageTarget) error {
	if target.Kind != model.StorageDomainRepository {
		return nil
	}
	server, err := s.store.GetServer(ctx, target.ServerID)
	if err != nil {
		return badRequest("подключение движка: %v", err)
	}
	if !server.Kind.UsesOVirtAPI() || !server.Enabled {
		return badRequest("нужно включённое подключение oVirt, РЕД Виртуализация, RHV или OLVM")
	}
	client, err := s.pool.ForServer(server)
	if err != nil {
		return badRequest("подключение движка: %v", err)
	}
	domain, err := client.GetStorageDomain(ctx, target.StorageDomainID)
	if err != nil {
		return badRequest("домен хранения: %v", err)
	}
	if domain.Type != "data" || domain.EffectiveStatus() != "active" {
		return badRequest("выберите активный домен данных, подключённый к движку")
	}
	return nil
}
