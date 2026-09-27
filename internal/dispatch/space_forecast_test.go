package dispatch

import (
	"testing"
	"time"
)

func TestScratchFreeCacheExpires(t *testing.T) {
	var c scratchFreeCache
	if _, ok := c.get("kvm\x00/var/lib/libvirt/qemu"); ok {
		t.Fatal("пустой кэш не должен отвечать")
	}

	c.put("kvm\x00/var/lib/libvirt/qemu", scratchReading{free: 42, at: time.Now().UTC()})
	if r, ok := c.get("kvm\x00/var/lib/libvirt/qemu"); !ok || r.free != 42 {
		t.Fatalf("свежий замер: %+v ok=%v", r, ok)
	}
	if _, ok := c.get("kvm\x00/srv/scratch"); ok {
		t.Fatal("другой каталог того же подключения — другой замер")
	}

	c.put("kvm\x00/var/lib/libvirt/qemu", scratchReading{free: 42, at: time.Now().UTC().Add(-scratchFreeTTL)})
	if _, ok := c.get("kvm\x00/var/lib/libvirt/qemu"); ok {
		t.Fatal("устаревший замер нужно повторить")
	}
}
