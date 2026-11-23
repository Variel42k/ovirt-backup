package ovirt

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Управление питанием ВМ.
//
// oVirt принимает такие действия через POST .../action с телом JSON. Движок отвечает сразу,
// а состояние меняется асинхронно. Результат проверяем позже через WaitVMStatus.

// StartVM загружает остановленную ВМ. На ВМ, которая приостановлена (в том числе
// приостановлена из-за ошибки I/O), тот же вызов возобновляет ее, что именно то, что нужно
// движку автоматического восстановления.
func (c *Client) StartVM(ctx context.Context, vmID string) error {
	return c.post(ctx, "/vms/"+vmID+"/start", struct{}{}, nil)
}

// StartVMOnHost просит движок запуститься на выбранном оператором узле.
// Пустой хост оставляет место для планировщика движка.
func (c *Client) StartVMOnHost(ctx context.Context, vmID, hostID string) error {
	if hostID == "" {
		return c.StartVM(ctx, vmID)
	}
	return c.post(ctx, "/vms/"+vmID+"/start", map[string]any{"vm": map[string]any{
		"placement_policy": map[string]any{"affinity": "pinned", "hosts": map[string]any{"host": []map[string]string{{"id": hostID}}}},
	}}, nil)
}

// ShutdownVM просит гостя чисто выключить ВМ. Требуется гостевой агент или ACPI поддержка;
// Если ни один из ответов не отвечает, ВМ просто остается.
func (c *Client) ShutdownVM(ctx context.Context, vmID string) error {
	return c.post(ctx, "/vms/"+vmID+"/shutdown", struct{}{}, nil)
}

// StopVM отключает питание ВМ. Потеря данных внутри гостя возможна.
func (c *Client) StopVM(ctx context.Context, vmID string) error {
	return c.post(ctx, "/vms/"+vmID+"/stop", struct{}{}, nil)
}

// SuspendVM сохраняет состояние ВМ в хранилище и прекращает выполнение.
func (c *Client) SuspendVM(ctx context.Context, vmID string) error {
	return c.post(ctx, "/vms/"+vmID+"/suspend", struct{}{}, nil)
}

// RebootVM попроси́ть гостя перезапустить.
func (c *Client) RebootVM(ctx context.Context, vmID string) error {
	return c.post(ctx, "/vms/"+vmID+"/reboot", struct{}{}, nil)
}

// ResetVM выполняет жесткий сброс, эквивалентный кнопке сброса.
func (c *Client) ResetVM(ctx context.Context, vmID string) error {
	return c.post(ctx, "/vms/"+vmID+"/reset", struct{}{}, nil)
}

// MigrateVM переносит запущенную ВМ на другой хост. Пустой hostID позволяет выбирать
// планировщику.
func (c *Client) MigrateVM(ctx context.Context, vmID, hostID string) error {
	body := map[string]any{}
	if hostID != "" {
		body["host"] = map[string]string{"id": hostID}
	}
	return c.post(ctx, "/vms/"+vmID+"/migrate", body, nil)
}

// VMStatus считывает только текущее состояние ВМ.
func (c *Client) VMStatus(ctx context.Context, vmID string) (status, detail string, err error) {
	var vm VM
	if err := c.get(ctx, "/vms/"+vmID, &vm); err != nil {
		return "", "", err
	}
	return vm.Status, vm.StatusDetail, nil
}

// GetVMState читает ВМ без подгрузки дисков: состояние и то, что сообщил
// гостевой агент. Для частого опроса, где GetVM с дисками избыточен.
func (c *Client) GetVMState(ctx context.Context, vmID string) (*VM, error) {
	var vm VM
	if err := c.get(ctx, "/vms/"+vmID, &vm); err != nil {
		return nil, err
	}
	return &vm, nil
}

func (c *Client) GetVMRecoveryState(ctx context.Context, vmID string) (*VM, error) {
	var vm VM
	if err := c.get(ctx, "/vms/"+vmID, &vm, withQuery(url.Values{"follow": {"reported_devices"}})); err != nil {
		return nil, err
	}
	return &vm, nil
}

// SearchVMs ищет ВМ запросом поиска движка, например name=jhv-verify-*.
func (c *Client) SearchVMs(ctx context.Context, query string) ([]VM, error) {
	var list vmList
	if err := c.get(ctx, "/vms", &list, withQuery(url.Values{"search": {query}})); err != nil {
		return nil, err
	}
	return list.VM, nil
}

// WaitVMStatus опрашивает ВМ до перехода в одно из заданных состояний.
func (c *Client) WaitVMStatus(ctx context.Context, vmID string, wanted []string, timeout time.Duration) (string, error) {
	want := map[string]bool{}
	for _, w := range wanted {
		want[w] = true
	}
	deadline := time.Now().Add(timeout)

	for {
		status, _, err := c.VMStatus(ctx, vmID)
		if err != nil {
			return "", err
		}
		if want[status] {
			return status, nil
		}
		if time.Now().After(deadline) {
			return status, fmt.Errorf("ВМ %s не перешла в состояние %v за %s (текущее: %s)",
				vmID, wanted, timeout, status)
		}
		select {
		case <-ctx.Done():
			return status, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// FreezeFilesystems просит гостевого агента успокоить файловые системы, чтобы снапшот или
// резервная копия были совместимы с приложением, а не сбоями.
func (c *Client) FreezeFilesystems(ctx context.Context, vmID string) error {
	return c.post(ctx, "/vms/"+vmID+"/freezefilesystems", struct{}{}, nil)
}

// FreezeMayBePending сообщает, что после такой ошибки заморозки гость мог
// всё же оказаться замороженным: движок не ответил вовсе или VDSM не дождался
// агента, а агент закончил заморозку позже. Отказ, в котором движок назвал
// причину (упавший сценарий, выключенная ВМ), — не такой случай.
func FreezeMayBePending(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return true
	}
	msg := strings.ToLower(apiErr.Error())
	for _, marker := range []string{"not responding", "timeout", "timed out", "not available"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// ThawFilesystems высвобождает заморозку. Он должен быть вызван даже тогда, когда операция
// между ними не удалась, в противном случае гость остается замороженным.
func (c *Client) ThawFilesystems(ctx context.Context, vmID string) error {
	return c.post(ctx, "/vms/"+vmID+"/thawfilesystems", struct{}{}, nil)
}

// Управление хостом.

// ActivateHost выводит хост из режима обслуживания.
func (c *Client) ActivateHost(ctx context.Context, hostID string) error {
	return c.post(ctx, "/hosts/"+hostID+"/activate", struct{}{}, nil)
}

// DeactivateHost переводит хост в режим обслуживания, перенося его ВМ подальше.
func (c *Client) DeactivateHost(ctx context.Context, hostID string) error {
	return c.post(ctx, "/hosts/"+hostID+"/deactivate", struct{}{}, nil)
}

// FenceHost Управля́ние питанием хоста. fenceType — один из вариантов запуска, остановки,
// перезапуска, состояния или руководства.
//
// Перезапуск останавливает все ВМ на хосте. Перед вызовом нужно подтвердить недоступность
// хоста и явное согласие оператора.
func (c *Client) FenceHost(ctx context.Context, hostID, fenceType string) error {
	switch fenceType {
	case "start", "stop", "restart", "status", "manual":
	default:
		return fmt.Errorf("недопустимый тип fence: %q", fenceType)
	}
	return c.post(ctx, "/hosts/"+hostID+"/fence", action{FenceType: fenceType}, nil)
}

// HostStatus читает только текущий статус хоста.
func (c *Client) HostStatus(ctx context.Context, hostID string) (string, error) {
	var h Host
	if err := c.get(ctx, "/hosts/"+hostID, &h); err != nil {
		return "", err
	}
	return h.Status, nil
}

// WaitHostStatus опрашивает хост до перехода в одно из заданных состояний.
func (c *Client) WaitHostStatus(ctx context.Context, hostID string, wanted []string, timeout time.Duration) (string, error) {
	want := map[string]bool{}
	for _, w := range wanted {
		want[w] = true
	}
	deadline := time.Now().Add(timeout)

	for {
		status, err := c.HostStatus(ctx, hostID)
		if err != nil {
			return "", err
		}
		if want[status] {
			return status, nil
		}
		if time.Now().After(deadline) {
			return status, fmt.Errorf("хост %s не перешёл в состояние %v за %s (текущее: %s)",
				hostID, wanted, timeout, status)
		}
		select {
		case <-ctx.Done():
			return status, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// Управление дисками.

// CreateDisk создаёт диск в домене хранения. Используется при восстановлении в новый диск.
func (c *Client) CreateDisk(ctx context.Context, req CreateDiskRequest) (*Disk, error) {
	if req.StorageDomainID == "" {
		return nil, errors.New("не указан домен хранения")
	}
	if req.ProvisionedSize <= 0 {
		return nil, errors.New("не указан размер диска")
	}
	format := req.Format
	if format == "" {
		format = "cow"
	}

	body := map[string]any{
		"name":             req.Alias,
		"alias":            req.Alias,
		"description":      req.Description,
		"provisioned_size": fmt.Sprint(req.ProvisionedSize),
		"format":           format,
		"sparse":           req.Sparse,
		"storage_domains": map[string]any{
			"storage_domain": []map[string]string{{"id": req.StorageDomainID}},
		},
	}
	if req.Incremental {
		body["backup"] = "incremental"
	}
	if req.InitialSize > 0 {
		body["initial_size"] = fmt.Sprint(req.InitialSize)
	}

	var disk Disk
	if err := c.post(ctx, "/disks", body, &disk); err != nil {
		return nil, err
	}
	return &disk, nil
}

// CreateDiskRequest описывает диск для обеспечения.
type CreateDiskRequest struct {
	Alias           string
	Description     string
	StorageDomainID string
	ProvisionedSize int64
	Format          string // cow | raw
	Sparse          bool
	Incremental     bool
	// InitialSize — начальный размер тома тонкого qcow2 на блочном домене.
	// Во время загрузки через imageio том никто не расширяет (ВМ не
	// запущена), и без него запись упирается в конец маленького начального
	// тома: imageio отвечает HTTP 500. Ноль — умолчание движка.
	InitialSize int64
}

// WaitDiskStatus опросы до тех пор, пока диск не покинет заблокированное состояние.
func (c *Client) WaitDiskStatus(ctx context.Context, diskID, wanted string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		d, err := c.GetDisk(ctx, diskID)
		if err != nil {
			return err
		}
		if d.Status == wanted {
			return nil
		}
		if d.Status == "illegal" {
			return fmt.Errorf("диск %s перешёл в состояние illegal", diskID)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("диск %s не перешёл в состояние %q за %s (текущее: %s)",
				diskID, wanted, timeout, d.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// AttachDisk привязывает существующий диск к ВМ.
func (c *Client) AttachDisk(ctx context.Context, vmID, diskID, iface string, bootable bool) error {
	if iface == "" {
		iface = "virtio_scsi"
	}
	body := map[string]any{
		"disk":      map[string]string{"id": diskID},
		"interface": iface,
		"bootable":  bootable,
		"active":    true,
	}
	return c.post(ctx, "/vms/"+vmID+"/diskattachments", body, nil)
}

// AttachDiskWhenUnlocked связывает диск с ВМ и выжидает блокировку движка на диске.
//
// После загрузки движок показывает status=ok и конечную фазу ImageTransfer
// раньше, чем снимает блокировку своей команды: подключение, отправленное
// сразу, получает 409 «Disk is locked. Please try again later». На РЕД
// Виртуализации 7.3 так пропала проверка, записавшая 300 ГиБ за пять часов:
// отказ пришёл через 25 мс после того, как диск стал ok. Состояние диска
// этой блокировки не показывает, поэтому остаётся повторять сам запрос.
//
// Повторяется только отказ из-за блокировки. Остальные 409 — диск уже
// подключён, ВМ в неподходящем состоянии — ожиданием не лечатся и
// возвращаются сразу. onLocked вызывается перед каждым ожиданием.
func (c *Client) AttachDiskWhenUnlocked(ctx context.Context, vmID, diskID, iface string, bootable bool,
	timeout time.Duration, onLocked func(attempt int, err error)) error {
	return c.attachDiskWhenUnlocked(ctx, vmID, diskID, iface, bootable, timeout, 3*time.Second, onLocked)
}

func (c *Client) attachDiskWhenUnlocked(ctx context.Context, vmID, diskID, iface string, bootable bool,
	timeout, interval time.Duration, onLocked func(attempt int, err error)) error {
	deadline := time.Now().Add(timeout)
	for attempt := 1; ; attempt++ {
		err := c.AttachDisk(ctx, vmID, diskID, iface, bootable)
		if err == nil || !IsLockConflict(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("движок не снял блокировку диска за %s: %w", timeout, err)
		}
		if onLocked != nil {
			onLocked(attempt, err)
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

// DeleteDisk удаляет диск навсегда.
func (c *Client) DeleteDisk(ctx context.Context, diskID string) error {
	return c.del(ctx, "/disks/"+diskID)
}
