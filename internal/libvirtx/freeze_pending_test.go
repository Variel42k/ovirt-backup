package libvirtx

import (
	"errors"
	"fmt"
	"testing"

	"github.com/digitalocean/go-libvirt"
)

func TestFreezeMayBePending(t *testing.T) {
	wrap := func(code libvirt.ErrorNumber, msg string) error {
		return fmt.Errorf("заморозка файловых систем гостя: %w", libvirt.Error{Code: uint32(code), Message: msg})
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"libvirt не дождался агента", wrap(libvirt.ErrAgentUnresponsive,
			"Guest agent is not responding: Guest agent not available for now"), true},
		{"тайм-аут команды агента", wrap(libvirt.ErrAgentCommandTimeout, "agent command timed out"), true},
		{"агент не подключён — заморозки не было", wrap(libvirt.ErrAgentUnresponsive,
			"Guest agent is not responding: QEMU guest agent is not connected"), false},
		{"агент отказал: упал сценарий", wrap(libvirt.ErrInternalError,
			"unable to execute QEMU agent command 'guest-fsfreeze-freeze': fsfreeze hook has failed with status 1"), false},
		{"оборвалась связь с libvirtd — неизвестно", errors.New("connection reset by peer"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FreezeMayBePending(tc.err); got != tc.want {
				t.Errorf("FreezeMayBePending = %v, ожидалось %v", got, tc.want)
			}
		})
	}
}
