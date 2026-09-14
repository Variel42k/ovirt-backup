package monitor

import (
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestManualResetIsAllowedOnlyForExplicitOperatorAction(t *testing.T) {
	r := &Remediator{cfg: config.RemediationConfig{AllowVMStart: true}}

	if r.actionAllowed(Situation{Action: model.ActionVMReset}) {
		t.Fatal("автоматика получила право на vm_reset")
	}
	if !r.actionAllowed(Situation{Action: model.ActionVMReset, Force: true}) {
		t.Fatal("явно подтверждённый ручной vm_reset заблокирован")
	}
	if !r.actionAllowed(Situation{Action: model.ActionVMStart}) {
		t.Fatal("разрешённый политикой vm_start заблокирован")
	}
	if r.actionAllowed(Situation{Action: model.ActionHostFence, Force: true}) {
		t.Fatal("Force обошёл запрет политики для host_fence")
	}
}
