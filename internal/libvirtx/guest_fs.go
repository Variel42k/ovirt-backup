package libvirtx

import (
	"context"
	"fmt"
	"sort"

	"github.com/digitalocean/go-libvirt"
)

// GuestFilesystem — смонтированная файловая система гостя по данным агента.
type GuestFilesystem struct {
	Mountpoint string `json:"mountpoint"`
	Type       string `json:"type"`
	Device     string `json:"device"`
	// Disks — устройства ВМ, на которых она лежит (vda, sdb).
	Disks []string `json:"disks,omitempty"`
}

// GuestFilesystems спрашивает у гостевого агента смонтированные файловые
// системы (guest-get-fsinfo), чтобы выборочную заморозку можно было задать
// выбором, а не вводом путей наугад: не точку монтирования агент молча
// пропускает.
//
// Только чтение: гость не замораживается. Вызовы go-libvirt контекста не
// принимают, поэтому ожидание ограничено снаружи — зависший агент не держит
// страницу, а сам вызов закончится, когда ответит libvirt.
func (c *Conn) GuestFilesystems(ctx context.Context, dom libvirt.Domain) ([]GuestFilesystem, error) {
	type result struct {
		info []libvirt.DomainFsinfo
		err  error
	}
	done := make(chan result, 1)
	go func() {
		info, _, err := c.lv.DomainGetFsinfo(dom, 0)
		done <- result{info, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return nil, fmt.Errorf("список файловых систем гостя: %w", r.err)
		}
		return guestFilesystems(r.info), nil
	case <-ctx.Done():
		return nil, fmt.Errorf("список файловых систем гостя: агент не ответил: %w", ctx.Err())
	}
}

func guestFilesystems(info []libvirt.DomainFsinfo) []GuestFilesystem {
	out := make([]GuestFilesystem, 0, len(info))
	seen := map[string]bool{}
	for _, fs := range info {
		if fs.Mountpoint == "" || seen[fs.Mountpoint] {
			continue
		}
		seen[fs.Mountpoint] = true
		out = append(out, GuestFilesystem{
			Mountpoint: fs.Mountpoint, Type: fs.Fstype, Device: fs.Name, Disks: fs.DevAliases,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mountpoint < out[j].Mountpoint })
	return out
}
