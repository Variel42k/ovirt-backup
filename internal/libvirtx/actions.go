package libvirtx

import (
	"context"
	"fmt"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
)

// Управление питанием для libvirt доменов.
//
// libvirt запускает остановленный домен через DomainCreate, а приостановленный возобновляет
// через DomainResume. StartDomain определяет состояние и выбирает нужный вызов.

// StartDomain поостанови́ть остановленный доменъ или выпустить остановленный.
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

// ShutdownDomain просит гостя чисто выключить ВМ. Требуется ACPI или гостевой агент; Без
// этого домен просто продолжает работать.
func (c *Conn) ShutdownDomain(ctx context.Context, dom golibvirt.Domain) error {
	if err := c.lv.DomainShutdown(dom); err != nil {
		return fmt.Errorf("штатное выключение домена %s: %w", dom.Name, err)
	}
	return nil
}

// DestroyDomain поотключа́ет власть домену. Потеря данных внутри гостя возможна, поэтому
// клиенты этого интерфейса закрывают его за явным подтверждением.
func (c *Conn) DestroyDomain(ctx context.Context, dom golibvirt.Domain) error {
	if err := c.lv.DomainDestroy(dom); err != nil {
		return fmt.Errorf("выключение питания домена %s: %w", dom.Name, err)
	}
	return nil
}

// SuspendDomain приостанавлива́ет казнь, держа гостя в памяти.
func (c *Conn) SuspendDomain(ctx context.Context, dom golibvirt.Domain) error {
	if err := c.lv.DomainSuspend(dom); err != nil {
		return fmt.Errorf("приостановка домена %s: %w", dom.Name, err)
	}
	return nil
}

// RebootDomain попроси́ть гостя перезапустить.
func (c *Conn) RebootDomain(ctx context.Context, dom golibvirt.Domain) error {
	if err := c.lv.DomainReboot(dom, 0); err != nil {
		return fmt.Errorf("перезагрузка домена %s: %w", dom.Name, err)
	}
	return nil
}

// ResetDomain выполняет жесткий сброс, эквивалентный кнопке сброса.
func (c *Conn) ResetDomain(ctx context.Context, dom golibvirt.Domain) error {
	if err := c.lv.DomainReset(dom, 0); err != nil {
		return fmt.Errorf("аппаратный сброс домена %s: %w", dom.Name, err)
	}
	return nil
}

// DomainState считывает текущее состояние домена.
func (c *Conn) DomainState(ctx context.Context, dom golibvirt.Domain) (State, error) {
	state, _, err := c.lv.DomainGetState(dom, 0)
	if err != nil {
		return StateUnknown, err
	}
	return stateFromCode(state), nil
}

// WaitDomainState опрашивает домен, пока он не перейдёт в одно из заданных состояний.
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

// HostInfo описывает сам гипервизор, который панель управления показывает как один хост —
// потому что для голого libvirt сервера это именно то, чем он является.
type HostInfo struct {
	Hostname  string
	CPUs      int
	MemoryKiB int64
	Version   string
	ActiveVMs int
	TotalVMs  int
}

// HostInfo собирает собственные виталы гипервизора.
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

	// NodeGetInfo возвращает аппаратные сведения: модель, память в КиБ, активные CPU, частоту,
	// узлы NUMA, сокеты, ядра и потоки.
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
