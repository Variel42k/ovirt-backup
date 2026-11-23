package ovirt

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"
)

// ErrBackupFailed — движок сам перевёл бэкап в failed. При require_consistency
// это обычно значит, что не удалась заморозка гостя; вызывающий отличает этот
// случай от сетевых ошибок и 409.
var ErrBackupFailed = errors.New("движок перевёл бэкап в состояние failed")

// Backup API в oVirt 4.4 и новее поддерживает горячие инкрементальные бэкапы.
// Последовательность вызовов приведена ниже.
//
//   1. POST /vms/{vm}/backups передаёт список дисков и опорный checkpoint для инкремента.
//      Движок фиксирует точку во времени.
//   2. Ждём phase == "ready". В to_checkpoint_id находится идентификатор нового checkpoint.
//   3. Открываем передачи со ссылкой на бэкап. Для полного бэкапа читаем все диапазоны, для
//      инкремента — изменённые.
//   4. POST .../finalize снимает блокировку дисков. Вызываем его и после ошибки передачи.

// StartBackup запрашивает у движка открытие бэкапа выбранных дисков.
// fromCheckpointID выбирает опорный checkpoint для инкремента. Пустое значение означает
// полный бэкап.
//
// Если движок принял запрос, но ответ не разобрался, возвращаем ошибку вместе с бэкапом,
// чей идентификатор удалось прочитать. Диски уже могут быть заблокированы, поэтому бэкап
// всё равно нужно завершить.
//
// requireConsistency передаёт движку require_consistency=true. Движок (4.4+)
// замораживает гостя сам — только на момент фиксации точки, после подготовки
// scratch-дисков или снапшота, — и с этим флагом проваливает бэкап, если
// заморозка не удалась. Без флага он тоже пробует заморозить, но при неудаче
// молча продолжает.
func (c *Client) StartBackup(ctx context.Context, vmID string, diskIDs []string, fromCheckpointID, description string,
	requireConsistency bool) (*Backup, error) {
	if len(diskIDs) == 0 {
		return nil, fmt.Errorf("не выбран ни один диск для бэкапа ВМ %s", vmID)
	}
	disks := make([]map[string]string, 0, len(diskIDs))
	for _, id := range diskIDs {
		disks = append(disks, map[string]string{"id": id})
	}
	body := map[string]any{
		"disks": map[string]any{"disk": disks},
	}
	if fromCheckpointID != "" {
		body["from_checkpoint_id"] = fromCheckpointID
	}
	if description != "" {
		body["description"] = description
	}

	var opts []func(*requestOptions)
	if requireConsistency {
		opts = append(opts, withQuery(url.Values{"require_consistency": {"true"}}))
	}

	var backup Backup
	if err := c.post(ctx, "/vms/"+vmID+"/backups", body, &backup, opts...); err != nil {
		// encoding/json заполняет доступные поля до сообщения о несовместимости типов.
		// Ошибка типа в другом поле не мешает получить ID.
		if backup.ID != "" {
			return &backup, err
		}
		return nil, err
	}
	return &backup, nil
}

// GetBackup читает текущее состояние бэкапа.
func (c *Client) GetBackup(ctx context.Context, vmID, backupID string) (*Backup, error) {
	var backup Backup
	if err := c.get(ctx, "/vms/"+vmID+"/backups/"+backupID, &backup); err != nil {
		return nil, err
	}
	return &backup, nil
}

// BackupDiskModes сообщает, как движок отдал каждый диск бэкапа: full или
// incremental. Пустая карта — движок режимов не сообщает (старше 4.4.5); тогда
// бэкап не смешанный, и режим у всех дисков один — тот, что запрошен.
func (c *Client) BackupDiskModes(ctx context.Context, vmID, backupID string) (map[string]string, error) {
	var list diskList
	if err := c.get(ctx, "/vms/"+vmID+"/backups/"+backupID+"/disks", &list); err != nil {
		return nil, err
	}
	modes := map[string]string{}
	for _, d := range list.Disk {
		if d.ID != "" && d.BackupMode != "" {
			modes[d.ID] = d.BackupMode
		}
	}
	return modes, nil
}

// ListBackups возвращает бэкапы ВМ, которые ещё учитывает движок, в том числе оставшиеся
// после сбоя.
func (c *Client) ListBackups(ctx context.Context, vmID string) ([]Backup, error) {
	var list backupList
	if err := c.get(ctx, "/vms/"+vmID+"/backups", &list); err != nil {
		return nil, err
	}
	return list.Backup, nil
}

// WaitBackupReady опрашивает состояние, пока бэкап не станет доступен для чтения или не
// завершится ошибкой.
func (c *Client) WaitBackupReady(ctx context.Context, vmID, backupID string, timeout time.Duration) (*Backup, error) {
	deadline := time.Now().Add(timeout)
	for {
		backup, err := c.GetBackup(ctx, vmID, backupID)
		if err != nil {
			return nil, err
		}
		switch backup.Phase {
		case "ready":
			return backup, nil
		case "failed":
			return backup, fmt.Errorf("бэкап %s: %w", backupID, ErrBackupFailed)
		case "succeeded":
			// Бэкап уже завершён другим процессом, читать больше нечего.
			return backup, fmt.Errorf("бэкап %s уже завершён движком", backupID)
		}
		if time.Now().After(deadline) {
			return backup, fmt.Errorf("бэкап %s не перешёл в состояние ready за %s (текущее: %s)",
				backupID, timeout, backup.Phase)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// BackupMarker описывает бэкап и связывает его с локальным запуском на случай потери ответа
// с идентификатором движка.
func BackupMarker(runID string) string { return "Backup run " + runID }

// SnapshotMarker формирует описание снапшота, созданного для локального запуска.
func SnapshotMarker(runID string) string { return "Backup snapshot " + runID }

// Open сообщает, что движок ещё держит бэкап: диски ВМ заблокированы, пока он
// не завершён.
func (b *Backup) Open() bool {
	switch b.Phase {
	case "succeeded", "failed":
		return false
	}
	return true
}

// ListImageTransfers возвращает все передачи образов движка. Фильтра по ВМ
// у коллекции нет: передачи отбираются по бэкапу или диску на стороне вызывающего.
func (c *Client) ListImageTransfers(ctx context.Context) ([]ImageTransfer, error) {
	var list struct {
		ImageTransfer []ImageTransfer `json:"image_transfer"`
	}
	if err := c.get(ctx, "/imagetransfers", &list); err != nil {
		return nil, err
	}
	return list.ImageTransfer, nil
}

// FinalizeBackup завершает бэкап и фиксирует его checkpoint. Вызываем для всех бэкапов,
// достигших "ready", включая завершившиеся ошибкой. Пока бэкап открыт, диски ВМ
// заблокированы и запустить следующий нельзя.
func (c *Client) FinalizeBackup(ctx context.Context, vmID, backupID string) error {
	return c.post(ctx, "/vms/"+vmID+"/backups/"+backupID+"/finalize", struct{}{}, nil)
}

// WaitBackupFinalized ждёт выхода бэкапа из finalizing, чтобы подтвердить снятие блокировки
// дисков.
func (c *Client) WaitBackupFinalized(ctx context.Context, vmID, backupID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		backup, err := c.GetBackup(ctx, vmID, backupID)
		if err != nil {
			// Некоторые версии движка убирают завершившиеся бэкапы из списка. Ответ 404
			// здесь означает, что бэкап уже завершился.
			if IsNotFound(err) {
				return nil
			}
			return err
		}
		switch backup.Phase {
		case "succeeded":
			return nil
		case "failed":
			return fmt.Errorf("бэкап %s завершился с ошибкой на стороне движка", backupID)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("бэкап %s не завершился за %s (текущее состояние: %s)",
				backupID, timeout, backup.Phase)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// ListCheckpoints возвращает checkpoint CBT, которые движок хранит для ВМ.
func (c *Client) ListCheckpoints(ctx context.Context, vmID string) ([]Checkpoint, error) {
	var list checkpointList
	if err := c.get(ctx, "/vms/"+vmID+"/checkpoints", &list); err != nil {
		return nil, err
	}
	return list.Checkpoint, nil
}

// HasCheckpoint проверяет, остался ли checkpoint в движке.
// При отсутствии checkpoint движок больше не может вычислить изменения, поэтому требуется
// полный бэкап.
func (c *Client) HasCheckpoint(ctx context.Context, vmID, checkpointID string) (bool, error) {
	if checkpointID == "" {
		return false, nil
	}
	checkpoints, err := c.ListCheckpoints(ctx, vmID)
	if err != nil {
		return false, err
	}
	for _, cp := range checkpoints {
		if cp.ID == checkpointID {
			return true, nil
		}
	}
	return false, nil
}

// DeleteCheckpoint удаляет checkpoint. Удаление начала цепочки делает недоступными
// зависимые инкременты. Удаляем только checkpoint, чьи бэкапы уже удалены по правилам
// хранения.
func (c *Client) DeleteCheckpoint(ctx context.Context, vmID, checkpointID string) error {
	return c.del(ctx, "/vms/"+vmID+"/checkpoints/"+checkpointID)
}

// Бэкап через снапшот для дисков и движков без отслеживания изменённых блоков.

// CreateSnapshot создаёт снапшот ВМ. При persistMemory=false сохраняются только диски.
// Сохранение памяти занимает больше времени и для согласованного образа диска не требуется.
func (c *Client) CreateSnapshot(ctx context.Context, vmID, description string, persistMemory bool, diskIDs []string) (*Snapshot, error) {
	body := map[string]any{
		"description":         description,
		"persist_memorystate": persistMemory,
	}
	if len(diskIDs) > 0 {
		attachments := make([]map[string]any, 0, len(diskIDs))
		for _, id := range diskIDs {
			attachments = append(attachments, map[string]any{"disk": map[string]string{"id": id}})
		}
		body["disk_attachments"] = map[string]any{"disk_attachment": attachments}
	}

	var snap Snapshot
	if err := c.post(ctx, "/vms/"+vmID+"/snapshots", body, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// GetSnapshot читает сведения об одном снапшоте.
func (c *Client) GetSnapshot(ctx context.Context, vmID, snapshotID string) (*Snapshot, error) {
	var snap Snapshot
	if err := c.get(ctx, "/vms/"+vmID+"/snapshots/"+snapshotID, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// WaitSnapshotReady опрашивает снапшот до выхода из состояния locked.
func (c *Client) WaitSnapshotReady(ctx context.Context, vmID, snapshotID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		snap, err := c.GetSnapshot(ctx, vmID, snapshotID)
		if err != nil {
			return err
		}
		if snap.SnapshotStatus == "ok" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("снапшот %s не создан за %s (состояние: %s)",
				snapshotID, timeout, snap.SnapshotStatus)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// ListSnapshotDisks возвращает диски снапшота. Поле image_id идентифицирует снапшот диска
// для передачи образа.
func (c *Client) ListSnapshotDisks(ctx context.Context, vmID, snapshotID string) ([]Disk, error) {
	var list diskList
	if err := c.get(ctx, "/vms/"+vmID+"/snapshots/"+snapshotID+"/disks", &list); err != nil {
		return nil, err
	}
	return list.Disk, nil
}

// ListSnapshots возвращает снапшоты ВМ, включая "Active VM", который движок добавляет
// всегда.
func (c *Client) ListSnapshots(ctx context.Context, vmID string) ([]Snapshot, error) {
	var list snapshotList
	if err := c.get(ctx, "/vms/"+vmID+"/snapshots", &list); err != nil {
		return nil, err
	}
	return list.Snapshot, nil
}

// DeleteSnapshot запрашивает удаление снапшота со слиянием слоёв. Слияние выполняется на
// гипервизоре и может продолжаться после возврата из вызова.
func (c *Client) DeleteSnapshot(ctx context.Context, vmID, snapshotID string) error {
	return c.del(ctx, "/vms/"+vmID+"/snapshots/"+snapshotID)
}

// DeleteSnapshotWhenReady повторяет запрос при временном 409, пока движок снимает
// блокировку после передачи образа. Ошибки аутентификации, связи и идентификаторов
// возвращаются сразу.
func (c *Client) DeleteSnapshotWhenReady(ctx context.Context, vmID, snapshotID string, timeout time.Duration) error {
	return c.deleteSnapshotWhenReady(ctx, vmID, snapshotID, timeout, 5*time.Second)
}

func (c *Client) deleteSnapshotWhenReady(ctx context.Context, vmID, snapshotID string,
	timeout, interval time.Duration) error {
	return retryConflict(ctx, timeout, interval, func() error {
		return c.DeleteSnapshot(ctx, vmID, snapshotID)
	})
}

func retryConflict(ctx context.Context, timeout, interval time.Duration, operation func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := operation()
		if err == nil || !IsConflict(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("engine не снял блокировку за %s: %w", timeout, err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// WaitSnapshotGone опрашивает ВМ, пока удалённый снапшот не исчезнет из списка.
func (c *Client) WaitSnapshotGone(ctx context.Context, vmID, snapshotID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, err := c.GetSnapshot(ctx, vmID, snapshotID)
		if IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("снапшот %s не удалён за %s; слияние данных может ещё идти на гипервизоре",
				snapshotID, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// ExportVMToOVA запрашивает у движка запись OVA ВМ на локальную файловую систему хоста.
// Такой экспорт самодостаточен, требует свободного места на хосте и занимает больше
// времени.
func (c *Client) ExportVMToOVA(ctx context.Context, vmID, hostID, directory, filename string) error {
	body := map[string]any{
		"host":      map[string]string{"id": hostID},
		"directory": directory,
		"filename":  filename,
	}
	return c.post(ctx, "/vms/"+vmID+"/exporttopathonhost", body, nil)
}

// VMConfiguration возвращает описание ВМ в формате OVF движка. Оно сохраняется при бэкапе
// только конфигурации.
func (c *Client) VMConfiguration(ctx context.Context, vmID string) (string, error) {
	q := url.Values{}
	q.Set("follow", "disk_attachments.disk,nics,tags,graphics_consoles")

	var vm map[string]any
	if err := c.get(ctx, "/vms/"+vmID, &vm, withQuery(q)); err != nil {
		return "", err
	}
	body, err := compactJSON(vm)
	if err != nil {
		return "", err
	}
	return string(body), nil
}
