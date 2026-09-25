package dispatch

import (
	"fmt"

	"github.com/Variel42k/ovirt-backup/internal/proxmox"
)

// proxmoxBackupOptions решает, с какими параметрами запускать vzdump.
//
// Оба параметра нужны, чтобы бэкап не мешал работающему гостю:
//
//   - предел чтения (--bwlimit) — тот же предел, что у задания на oVirt и KVM,
//     в КиБ/с, как его понимает vzdump;
//   - fleecing — пока диск не прочитан, гость, перезаписывая блок, ждёт, пока
//     старое содержимое уйдёт к получателю копии. Здесь получатель — поток по
//     SSH до службы, и при медленной сети гость тормозит вместе с ним. С
//     fleecing старые блоки ложатся в локальный образ на узле, и запись гостя
//     сети не ждёт.
//
// Что применить нельзя, не роняет бэкап: он идёт как раньше, а причина
// возвращается в skipped для журнала и хронологии. applied — что применено.
func proxmoxBackupOptions(fleecingStorage, vmID string, limitMBps int,
	caps proxmox.DataPlaneCaps) (opts proxmox.BackupOptions, applied, skipped []string) {

	const updateHelper = "обновите jhvirt-pve-data-plane на узлах до версии из этого комплекта"

	if limitMBps > 0 {
		if caps.Options() {
			opts.BandwidthKiB = int64(limitMBps) * 1024
			applied = append(applied, fmt.Sprintf("предел чтения %d МиБ/с", limitMBps))
		} else {
			skipped = append(skipped, "предел чтения не применён: помощник на узле его не поддерживает — "+updateHelper)
		}
	}

	if fleecingStorage == "" {
		return opts, applied, skipped
	}
	kind, _, err := proxmox.ParseVMID(vmID)
	switch {
	case err != nil:
		skipped = append(skipped, "fleecing не применён: "+err.Error())
	case kind != "qemu":
		// У контейнера нет copy-before-write: vzdump снимает его снимком
		// хранилища, и fleecing ему не нужен.
	case !caps.Options():
		skipped = append(skipped, "fleecing не применён: помощник на узле его не поддерживает — "+updateHelper)
	case !caps.Fleecing:
		skipped = append(skipped, "fleecing не применён: на узле Proxmox VE старше 8.2")
	default:
		opts.FleecingStorage = fleecingStorage
		applied = append(applied, "fleecing в хранилище "+fleecingStorage)
	}
	return opts, applied, skipped
}
