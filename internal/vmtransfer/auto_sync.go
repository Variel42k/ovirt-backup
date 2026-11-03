package vmtransfer

import (
	"fmt"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func validateAutoSyncInterval(seconds int) error {
	if seconds == -1 || seconds == 0 || (seconds >= 60 && seconds <= 86400) {
		return nil
	}
	return fmt.Errorf("период автосинхронизации: от 60 до 86400 секунд или -1 для ручного режима")
}

func autoSyncDue(t *model.VMTransfer, now time.Time) bool {
	if t.Status != "ready" || t.Request.Mode != "warm" || t.ManualPause {
		return false
	}
	seconds := t.Request.AutoSyncIntervalSeconds
	if validateAutoSyncInterval(seconds) != nil || seconds == -1 {
		return false
	}
	if seconds == 0 {
		seconds = 120
	}
	return now.Sub(t.UpdatedAt) >= time.Duration(seconds)*time.Second
}
