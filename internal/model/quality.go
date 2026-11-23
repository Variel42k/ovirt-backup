package model

import "time"

// StorageUsageSample является одним наблюдением емкости репозитория. CapacityKnown это
// false для хранилищ объектов и SFTP серверов, которые не могут сообщить о квоте;
// Используемые байты все еще могут быть полезны для диаграммы роста.
type StorageUsageSample struct {
	ID              int64     `json:"id"`
	StorageTargetID string    `json:"storage_target_id"`
	CheckOK         bool      `json:"check_ok"`
	CapacityKnown   bool      `json:"capacity_known"`
	FreeBytes       int64     `json:"free_bytes"`
	UsedBytes       int64     `json:"used_bytes"`
	At              time.Time `json:"at"`
}
