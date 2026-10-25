package guesthealth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"time"
)

var ErrNoAddress = errors.New("нет пригодного IP гостя; автоматический сброс по сетевой проверке запрещён")

// CheckTCP checks a guest-reported address. One shared deadline bounds all
// addresses; lack of addresses is an unknown result, not proof of a hung guest.
func CheckTCP(ctx context.Context, addresses []string, port int, timeout time.Duration) error {
	if port == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var targets []string
	seen := map[netip.Addr]bool{}
	for _, ip := range addresses {
		a, e := netip.ParseAddr(ip)
		a = a.Unmap()
		if e == nil && a.IsGlobalUnicast() && !a.IsLoopback() && !a.IsLinkLocalUnicast() && !seen[a] {
			seen[a] = true
			targets = append(targets, net.JoinHostPort(a.String(), strconv.Itoa(port)))
		}
	}
	if len(targets) > 64 {
		return fmt.Errorf("%w: слишком много адресов для достоверной проверки", ErrNoAddress)
	}
	if len(targets) == 0 {
		return ErrNoAddress
	}
	result := make(chan error, len(targets))
	for _, target := range targets {
		go func(addr string) {
			c, e := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
			if e == nil {
				_ = c.Close()
			}
			result <- e
		}(target)
	}
	var last error
	for range targets {
		select {
		case err := <-result:
			if err == nil {
				return nil
			}
			last = err
		case <-ctx.Done():
			return fmt.Errorf("TCP %d: %w", port, ctx.Err())
		}
	}
	return fmt.Errorf("TCP %d недоступен: %w", port, last)
}
