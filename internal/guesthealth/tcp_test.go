package guesthealth

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestTCPUnknownIsNotAHungGuest(t *testing.T) {
	for _, ips := range [][]string{nil, {"127.0.0.1", "::1", "169.254.1.2", "fe80::1", "nonsense", "0.0.0.0"}} {
		if err := CheckTCP(context.Background(), ips, 22, time.Second); !errors.Is(err, ErrNoAddress) {
			t.Fatal("unknown address treated as failure", err)
		}
	}
	if err := CheckTCP(context.Background(), nil, 0, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestTCPTriesAllUniqueGuestAddressesAndBoundsTimeout(t *testing.T) {
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	own := ""
	for _, address := range addresses {
		ip, _, e := net.ParseCIDR(address.String())
		if e == nil && ip.To4() != nil && !ip.IsLoopback() && ip.IsGlobalUnicast() {
			own = ip.String()
			break
		}
	}
	if own == "" {
		t.Skip("no guest-like interface available")
	}
	port := listener.Addr().(*net.TCPAddr).Port
	ips := []string{"192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4", own, own}
	if err = CheckTCP(context.Background(), ips, port, time.Second); err != nil {
		t.Fatal("reachable guest after first four addresses ignored", err)
	}
	_ = listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	err = CheckTCP(ctx, []string{own}, port, time.Second)
	if err == nil || !strings.Contains(err.Error(), "TCP") || time.Since(started) > time.Second {
		t.Fatal("cancellation not bounded", err)
	}
}
