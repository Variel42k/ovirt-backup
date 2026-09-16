package ovirt

import (
	"context"
	"testing"
)

// SelectPermits оставляет существующие права и отдельно возвращает отброшенные;
// пустой каталог означает «не выяснить» — тогда набор проходит как есть.
func TestSelectPermits(t *testing.T) {
	catalog := map[string]bool{"login": true, "backup_disk": true, "access_image_storage": true}

	use, skipped := SelectPermits([]string{"login", "access_image_transfer", "backup_disk"}, catalog)
	if len(use) != 2 || use[0] != "login" || use[1] != "backup_disk" {
		t.Fatalf("ожидались login,backup_disk; получено %v", use)
	}
	if len(skipped) != 1 || skipped[0] != "access_image_transfer" {
		t.Fatalf("ожидалось отбросить access_image_transfer; получено %v", skipped)
	}

	// Пустой каталог: ничего не отбрасываем, отдаём как есть.
	use, skipped = SelectPermits([]string{"login", "whatever"}, nil)
	if len(use) != 2 || len(skipped) != 0 {
		t.Fatalf("пустой каталог должен пропускать всё: use=%v skipped=%v", use, skipped)
	}
}

// EnginePermits объединяет права по всем уровням кластера, встроенным в
// коллекцию /clusterlevels.
func TestEnginePermitsUnionsLevels(t *testing.T) {
	client := engineStub(t, map[string]any{
		"clusterlevels": `{"cluster_level":[
			{"id":"4.3","permits":{"permit":[
				{"name":"login"},{"name":"backup_disk"},{"name":"access_image_storage"},
				{"name":"manipulate_vm_snapshots"},{"name":"create_disk"},{"name":"delete_disk"},
				{"name":"configure_disk_storage"},{"name":"create_vm"},{"name":"edit_vm_properties"},
				{"name":"configure_vm_storage"}]}},
			{"id":"4.6","permits":{"permit":[
				{"name":"login"},{"name":"sparsify_disk"}]}}
		]}`,
	})

	catalog, err := client.EnginePermits(context.Background())
	if err != nil {
		t.Fatalf("каталог: %v", err)
	}
	// Объединение по уровням: право только с 4.6 тоже попадает в каталог.
	if !catalog["sparsify_disk"] {
		t.Fatalf("объединение по уровням не сработало: %v", catalog)
	}
	// Реальное имя есть, устаревшее — нет.
	if !catalog["access_image_storage"] || catalog["access_image_transfer"] {
		t.Fatalf("каталог должен знать access_image_storage и не знать access_image_transfer: %v", catalog)
	}

	// Весь набор по умолчанию должен резолвиться в этом каталоге без остатка —
	// это и есть защита от повторения бага с несуществующим именем.
	_, skipped := SelectPermits(DefaultBackupPermits, catalog)
	if len(skipped) != 0 {
		t.Fatalf("DefaultBackupPermits содержит имена вне каталога движка: %v", skipped)
	}
}

// DefaultBackupPermits не должен содержать несуществующее на движке имя, из-за
// которого движок отвергал бы всю роль.
func TestDefaultBackupPermitsUseRealNames(t *testing.T) {
	for _, p := range DefaultBackupPermits {
		if p == "access_image_transfer" {
			t.Fatal("access_image_transfer не существует на движке; нужно access_image_storage")
		}
	}
}
