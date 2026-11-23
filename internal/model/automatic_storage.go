package model

import "github.com/google/uuid"

// Стабильный по всем инвентарным опросам, переименованиям и домену, исчезающему /
// появляющемуся.
func AutomaticDomainStorageID(serverID, domainID string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("jhvirt:domain-repository:"+serverID+"\x00"+domainID)).String()
}

func (t *StorageTarget) IsAutomaticDomainStorage() bool {
	return t.Kind == StorageDomainRepository && t.ServerID != "" && t.StorageDomainID != "" &&
		t.ID == AutomaticDomainStorageID(t.ServerID, t.StorageDomainID)
}
