package libvirtx

import (
	"context"
	"fmt"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
)

// Power management for libvirt domains.
//
// libvirt splits what oVirt merges: a stopped domain is started with
// DomainCreate, while a paused one is released with DomainResume. Calling the
// wrong one fails, so StartDomain looks at the current state first — the
// caller should not have to know which of the two situations it is in.

// StartDomain boots a stopped domain or releases a paused one.
func (c *Conn) StartDomain(ctx context.Context, dom golibvirt.Domain) error {
	state, _, err := c.lv.DomainGetState(dom, 0)
	if err != nil {
		return fmt.Errorf("состояние домена %s: %w", dom.Name, err)
	}

	switch stateFromCode(state) {
	case StateRunning, StateBlocked:
		return nil // уже работает — не ошибка
	case StatePaused:
		if err := c.lv.DomainResume(dom); err != nil {
			return fmt.Errorf("снятие домена %s с паузы: %w", dom.Name, err)
		}
		return nil
	case StatePMSuspended:
		if err := c.lv.DomainPmWakeup(dom, 0); err != nil {
			return fmt.Errorf("пробуждение домена %s: %w", dom.Name, err)
		}
		return nil
	default:
		if err := c.lv.DomainCreate(dom); err != nil {
			return fmt.Errorf("запуск домена %s: %w", dom.Name, err)
		}
		return nil
	}
}

// ShutdownDomain asks the guest to power off cleanly. It needs ACPI or a guest
// agent; without either the domain simply keeps running.
func (c *Conn) ShutdownDomain(ctx context.Context, dom golibvirt.Domain) error {
	if err := c.lv.DomainShutdown(dom); err != nil {
		return fmt.Errorf("штатное выключение домена %s: %w", dom.Name, err)
	}
	return nil
}

// DestroyDomain cuts power to the domain. Data loss inside the guest is
// possible, which is why callers gate it behind an explicit confirmation.
func (c *Conn) DestroyDomain(ctx context.Context, dom golibvirt.Domain) error {
	if err := c.lv.DomainDestroy(dom); err != nil {
		return fmt.Errorf("выключение питания домена %s: %w", dom.Name, err)
	}
	return nil
}

// SuspendDomain pauses execution, keeping the guest in memory.
func (c *Conn) SuspendDomain(ctx context.Context, dom golibvirt.Domain) error {
	if err := c.lv.DomainSuspend(dom); err != nil {
		return fmt.Errorf("приостановка домена %s: %w", dom.Name, err)
	}
	return nil
}

// RebootDomain asks the guest to restart.
func (c *Conn) RebootDomain(ctx context.Context, dom golibvirt.Domain) error {
	if err := c.lv.DomainReboot(dom, 0); err != nil {
		return fmt.Errorf("перезагрузка домена %s: %w", dom.Name, err)
	}
	return nil
}

// ResetDomain performs a hard reset, equivalent to the reset button.
func (c *Conn) ResetDomain(ctx context.Context, dom golibvirt.Domain) error {
	if err := c.lv.DomainReset(dom, 0); err != nil {
		return fmt.Errorf("аппаратный сброс домена %s: %w", dom.Name, err)
	}
	return nil
}

// DomainState reads the current state of a domain.
func (c *Conn) DomainState(ctx context.Context, dom golibvirt.Domain) (State, error) {
	state, _, err := c.lv.DomainGetState(dom, 0)
	if err != nil {
		return StateUnknown, err
	}
	return stateFromCode(state), nil
}

// WaitDomainState polls until the domain reaches one of the wanted states.
func (c *Conn) WaitDomainState(ctx context.Context, dom golibvirt.Domain, wanted []State, timeout time.Duration) (State, error) {
	want := map[State]bool{}
	for _, w := range wanted {
		want[w] = true
	}
	deadline := time.Now().Add(timeout)

	for {
		state, err := c.DomainState(ctx, dom)
		if err != nil {
			return StateUnknown, err
		}
		if want[state] {
			return state, nil
		}
		if time.Now().After(deadline) {
			return state, fmt.Errorf("домен %s не перешёл в состояние %v за %s (текущее: %s)",
				dom.Name, wanted, timeout, state)
		}
		select {
		case <-ctx.Done():
			return state, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// HostInfo describes the hypervisor itself, which the dashboard shows as a
// single host — because for a bare libvirt server, that is exactly what it is.
type HostInfo struct {
	Hostname  string
	CPUs      int
	MemoryKiB int64
	Version   string
	ActiveVMs int
	TotalVMs  int
}

// HostInfo collects the hypervisor's own vitals.
func (c *Conn) HostInfo(ctx context.Context) (*HostInfo, error) {
	info := &HostInfo{}

	if hostname, err := c.lv.ConnectGetHostname(); err == nil {
		info.Hostname = hostname
	} else {
		info.Hostname = c.cfg.Host
	}
	if version, err := c.Version(ctx); err == nil {
		info.Version = version
	}

	// NodeGetInfo returns the physical machine's CPU and memory layout:
	// model, memory in KiB, active CPUs, MHz, NUMA nodes, sockets, cores,
	// threads.
	if _, memory, cpus, _, _, sockets, cores, threads, err := c.lv.NodeGetInfo(); err == nil {
		info.MemoryKiB = int64(memory)
		info.CPUs = int(cpus)
		if info.CPUs == 0 {
			info.CPUs = int(sockets) * int(cores) * int(threads)
		}
	}

	domains, _, err := c.lv.ConnectListAllDomains(1, 0)
	if err == nil {
		info.TotalVMs = len(domains)
		for _, dom := range domains {
			if state, _, err := c.lv.DomainGetState(dom, 0); err == nil {
				if stateFromCode(state).Running() {
					info.ActiveVMs++
				}
			}
		}
	}
	return info, nil
}
