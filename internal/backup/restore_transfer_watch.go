package backup

import (
	"net/url"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// restoreTransferWatch makes an apparently frozen restore diagnosable while
// it is still running. Progress alone cannot tell whether the service is
// waiting for the backup repository, imageio, or the storage-domain flush.
type restoreTransferWatch struct {
	log  zerolog.Logger
	stop chan struct{}
	done chan struct{}
	once sync.Once

	mu      sync.Mutex
	stage   string
	offset  int64
	length  int64
	started time.Time
}

func newRestoreTransferWatch(log zerolog.Logger, transferID, diskID, diskName, repository, dataURL string) *restoreTransferWatch {
	host := imageioNode(dataURL)
	w := &restoreTransferWatch{
		log: log.With().Str("transfer", transferID).Str("диск-id", diskID).
			Str("диск", diskName).Str("хранилище-копии", repository).
			Str("узел-imageio", host).Logger(),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go w.loop()
	return w
}

func imageioNode(dataURL string) string {
	if parsed, err := url.Parse(dataURL); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return "неизвестен"
}

func (w *restoreTransferWatch) Observe(stage string, offset, length int64) {
	w.mu.Lock()
	w.stage, w.offset, w.length, w.started = stage, offset, length, time.Now()
	w.mu.Unlock()
}

func (w *restoreTransferWatch) Stop() {
	w.once.Do(func() { close(w.stop) })
	<-w.done
}

func (w *restoreTransferWatch) loop() {
	defer close(w.done)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	var lastStage string
	var lastWarning time.Time
	for {
		select {
		case <-w.stop:
			return
		case now := <-ticker.C:
			w.mu.Lock()
			stage, offset, length, started := w.stage, w.offset, w.length, w.started
			w.mu.Unlock()
			if stage == "" || started.IsZero() || now.Sub(started) < 30*time.Second {
				continue
			}
			if stage != lastStage {
				lastStage, lastWarning = stage, time.Time{}
			}
			if !lastWarning.IsZero() && now.Sub(lastWarning) < 30*time.Second {
				continue
			}
			w.log.Warn().Str("операция", stage).Int64("смещение", offset).Int64("размер", length).
				Dur("без-завершения", now.Sub(started)).Str("что-проверить", restoreStageHint(stage)).
				Msg("операция передачи долго не завершается")
			lastWarning = now
		}
	}
}

func restoreStageHint(stage string) string {
	switch stage {
	case "reading_backup":
		return "доступность и задержки хранилища резервных копий"
	case "writing_imageio":
		return "ovirt-imageio на указанном узле, сеть до него и задержки домена хранения"
	case "imageio_flush":
		return "журнал ovirt-imageio и операции/свободное место домена хранения; Sent в движке во время flush не растёт"
	case "imageio_options":
		return "доступность ticket URL ovirt-imageio"
	default:
		return "журналы движка и ovirt-imageio"
	}
}
