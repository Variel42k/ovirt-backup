package ovirt

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestFreezeMayBePending(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"движок не ответил", fmt.Errorf("oVirt POST: %w", context.DeadlineExceeded), true},
		{"VDSM не дождался агента", &APIError{Status: 400, Method: "POST", Path: "/vms/1/freezefilesystems",
			Detail: "Guest agent is not responding"}, true},
		{"агент отказал: упал сценарий", &APIError{Status: 400, Method: "POST", Path: "/vms/1/freezefilesystems",
			Detail: "fsfreeze hook has failed with status 1"}, false},
		{"ВМ выключена", &APIError{Status: 409, Method: "POST", Path: "/vms/1/freezefilesystems",
			Detail: "Cannot freeze VM. VM is not running."}, false},
		{"обрыв связи", errors.New("connection reset by peer"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FreezeMayBePending(tc.err); got != tc.want {
				t.Errorf("FreezeMayBePending = %v, ожидалось %v", got, tc.want)
			}
		})
	}
}
