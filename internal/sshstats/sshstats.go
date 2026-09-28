// Package sshstats считает SSH-подключения службы к хостам.
//
// Каждое новое SSH-подключение оставляет строки в журнале хоста: вход,
// сессия PAM, выход. Пулы соединений держат подключения открытыми, чтобы
// серия операций обходилась одним входом. Счётчик показывает, так ли это на
// деле: число подключений к хосту должно расти редко, а не на каждую
// операцию. Его отдаёт метрика ovirt_backup_ssh_connections_total.
package sshstats

import (
	"sort"
	"sync"
)

// Кто подключается — значение метки component.
const (
	Libvirt   = "libvirt"   // соединение пула libvirt с хостом KVM
	DBDump    = "db-dump"   // помощник логических дампов на хосте СУБД
	Proxmox   = "proxmox"   // помощник канала данных на узле Proxmox
	SFTPRepo  = "sftp-repo" // хранилище копий SFTP
	HostKey   = "host-key"  // чтение ключа хоста без входа
	Discovery = "discovery" // ограниченный помощник инвентаризации гостя
)

type key struct{ host, component string }

type counts struct{ ok, failed uint64 }

var (
	mu    sync.Mutex
	seen  = map[key]*counts{}
	limit = 1000 // меток не больше, чем хостов разумно держать в службе
)

// Record отмечает SSH-подключение к host; err — итог рукопожатия.
func Record(host, component string, err error) {
	mu.Lock()
	defer mu.Unlock()
	k := key{host, component}
	c := seen[k]
	if c == nil {
		if len(seen) >= limit {
			return
		}
		c = &counts{}
		seen[k] = c
	}
	if err != nil {
		c.failed++
	} else {
		c.ok++
	}
}

// Sample — подключения к одному хосту от одного компонента.
type Sample struct {
	Host      string
	Component string
	Connected uint64
	Failed    uint64
}

// Snapshot — счётчики на этот момент, упорядоченные по хосту и компоненту.
func Snapshot() []Sample {
	mu.Lock()
	out := make([]Sample, 0, len(seen))
	for k, c := range seen {
		out = append(out, Sample{Host: k.host, Component: k.component, Connected: c.ok, Failed: c.failed})
	}
	mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].Component < out[j].Component
	})
	return out
}

// reset — для тестов.
func reset() {
	mu.Lock()
	seen = map[key]*counts{}
	mu.Unlock()
}
