package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/imageio"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/repo"
)

// Образ диска на движке без Backup API (oVirt 4.3).
//
// ovirt-imageio 1.x из oVirt 4.3 отдаёт файл тома как есть. У тома qcow2 это
// байты формата qcow2 одного слоя, а не содержимое диска: читать его
// диапазонами как сырой диск нельзя. Копия получилась бы неверной, а на
// коротком слое демон ещё и падает на конце файла с «HTTP 500: Server failed
// to perform the request». Превращать qcow2 в сырой поток imageio научился
// только в oVirt 4.4, через qemu-nbd.
//
// Поэтому том qcow2 служба собирает сама: скачивает цепочку томов снапшота
// целиком, связывает слои друг с другом через qemu-img и превращает верхний
// слой в сырой образ, который и раскладывает на чанки. Том raw без предков —
// это и есть диск; его по-прежнему читают диапазонами.

// legacyVolume — том снапшота, который собирается из цепочки.
type legacyVolume struct {
	ImageID string
	Format  string // cow | raw
}

// legacyChainDepth — сколько слоёв допускается в цепочке. Больше — почти
// наверняка ошибка разбора backing file, а не настоящая цепочка.
const legacyChainDepth = 128

// legacyVolumeDownloadAttempts bounds retries of a full-volume stream. oVirt
// 4.3 imageio cannot resume these downloads with Range, so every new ticket
// starts the current volume from byte zero.
const legacyVolumeDownloadAttempts = 3

var (
	legacyTransferCloseWait = 2 * time.Minute
	// legacyTransferFinalizeWait — сколько ждать завершения передачи после
	// успешного finalize: движок 4.3 снимает блокировку диска только после
	// этого, а следующий том цепочки — новая передача того же диска.
	legacyTransferFinalizeWait = 10 * time.Minute
	legacyVolumeRetryDelay     = 2 * time.Second
)

// errTransferNotReleased — движок не завершил передачу тома: диск остаётся
// заблокированным, и повтор скачивания только получил бы 409.
var errTransferNotReleased = errors.New("движок не освободил передачу тома")

func transferNotReleased(cause error, transferID, phase string, err error) error {
	if phase == "" {
		phase = "неизвестна"
	}
	released := fmt.Errorf("%w: передача %s не завершилась (фаза %s): %v. Пока она открыта, движок держит диск — "+
		"новая передача и удаление снапшота получат 409 «disks are locked»", errTransferNotReleased, transferID, phase, err)
	if cause == nil {
		return fmt.Errorf("том скачан, но %w", released)
	}
	return fmt.Errorf("%w; вдобавок %w", cause, released)
}

// snapshotVolumeFormats — формат каждого тома ВМ по её снапшотам. По нему
// видно, как скачивать том и надо ли искать у него предка. Снапшот, диски
// которого не прочитались, пропускается: если его том понадобится, сборка
// цепочки остановится с понятной ошибкой.
func snapshotVolumeFormats(ctx context.Context, client *ovirt.Client, vmID string) (map[string]string, error) {
	snapshots, err := client.ListSnapshots(ctx, vmID)
	if err != nil {
		return nil, fmt.Errorf("список снапшотов ВМ: %w", err)
	}
	formats := map[string]string{}
	for _, s := range snapshots {
		if s.SnapshotType == "active" {
			continue
		}
		disks, err := client.ListSnapshotDisks(ctx, vmID, s.ID)
		if err != nil {
			continue
		}
		for _, d := range disks {
			if d.ImageID != "" && d.Format != "" {
				formats[d.ImageID] = d.Format
			}
		}
	}
	return formats, nil
}

// needsLegacyChain — надо ли собирать диск из цепочки: движок без Backup API
// и том снапшота в формате qcow2.
func needsLegacyChain(srv *model.Server, volumeFormat string) bool {
	return !srv.SupportsCBT && volumeFormat == "cow"
}

// qemuFormat — имя формата oVirt в терминах qemu-img.
func qemuFormat(ovirtFormat string) string {
	if ovirtFormat == "cow" {
		return "qcow2"
	}
	return "raw"
}

// backingVolumeID — идентификатор тома из backing file слоя. oVirt пишет туда
// имя тома, иногда с относительным путём: «../<образ>/<том>» или «<том>».
func backingVolumeID(backing string) string {
	parts := strings.FieldsFunc(backing, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) == 0 {
		return ""
	}
	id := parts[len(parts)-1]
	id = strings.TrimSuffix(id, ".qcow2")
	return strings.TrimSuffix(id, ".raw")
}

// localVolume — скачанный том цепочки.
type localVolume struct {
	id, format, path string
}

// materializeLegacyChain собирает сырой образ диска на момент снапшота: top —
// том снапшота, formats — форматы томов ВМ. Возвращает путь к сырому образу и
// сколько байт скачано. Всё создаётся в workDir; вызывающий удаляет каталог.
func (e *Engine) materializeLegacyChain(ctx context.Context, client *ovirt.Client, top legacyVolume,
	formats map[string]string, workDir string, onDownload func(int64)) (string, int64, error) {

	var chain []localVolume
	var downloaded int64
	id, format := top.ImageID, top.Format
	for depth := 0; ; depth++ {
		if depth >= legacyChainDepth {
			return "", downloaded, fmt.Errorf("цепочка томов длиннее %d слоёв — похоже на ошибку разбора", legacyChainDepth)
		}
		path := filepath.Join(workDir, fmt.Sprintf("%03d-%s.%s", depth, id, qemuFormat(format)))
		n, err := e.downloadVolume(ctx, client, id, format, path, func(done int64) {
			if onDownload != nil {
				onDownload(downloaded + done)
			}
		})
		downloaded += n
		if err != nil {
			return "", downloaded, fmt.Errorf("скачивание тома %s: %w", id, err)
		}
		chain = append(chain, localVolume{id: id, format: format, path: path})
		if format != "cow" {
			break
		}
		backing, err := Qcow2BackingFile(ctx, e.cfg.QemuImgPath, path)
		if err != nil {
			return "", downloaded, err
		}
		if backing == "" {
			break
		}
		next := backingVolumeID(backing)
		nextFormat := formats[next]
		if nextFormat == "" {
			return "", downloaded, fmt.Errorf("том %s ссылается на %q, но такого тома нет ни в одном снапшоте ВМ — "+
				"цепочку собрать нельзя", id, backing)
		}
		id, format = next, nextFormat
	}

	// Слои ссылаются на пути хоста oVirt, которых здесь нет. -u меняет только
	// ссылку: содержимое скачанного предка то же, что у тома на хосте.
	for i := len(chain) - 2; i >= 0; i-- {
		parent := chain[i+1]
		if err := RebaseQcow2Onto(ctx, e.cfg.QemuImgPath, chain[i].path, parent.path, qemuFormat(parent.format)); err != nil {
			return "", downloaded, err
		}
	}
	topVolume := chain[0]
	if topVolume.format != "cow" {
		// Том raw без предков — это и есть образ диска.
		return topVolume.path, downloaded, nil
	}
	rawPath := filepath.Join(workDir, "image.raw")
	if err := ConvertQcow2ToRaw(ctx, e.cfg.QemuImgPath, topVolume.path, rawPath); err != nil {
		return "", downloaded, err
	}
	for _, v := range chain {
		_ = os.Remove(v.path)
	}
	return rawPath, downloaded, nil
}

// downloadVolume скачивает том снапшота целиком. Нулевые блоки не пишутся, и
// файл остаётся разреженным: том raw занимает на диске службы столько, сколько
// в нём данных, а не весь размер диска.
func (e *Engine) downloadVolume(ctx context.Context, client *ovirt.Client, imageID, format, path string,
	onProgress func(int64)) (int64, error) {
	var lastN int64
	for attempt := 1; attempt <= legacyVolumeDownloadAttempts; attempt++ {
		n, err := e.downloadVolumeAttempt(ctx, client, imageID, format, path, onProgress)
		lastN = n
		if err == nil {
			return n, nil
		}
		// Не освобождённая движком передача держит диск: новая попытка
		// получила бы 409, повторять бессмысленно.
		if ctx.Err() != nil || errors.Is(err, errTransferNotReleased) || !imageio.IsNetworkError(err) {
			return n, err
		}
		if attempt == legacyVolumeDownloadAttempts {
			return n, fmt.Errorf("передача тома оборвалась после %d попыток; каждая попытка начиналась заново, так как imageio oVirt 4.3 не поддерживает продолжение: %w",
				legacyVolumeDownloadAttempts, err)
		}
		if onProgress != nil {
			onProgress(0)
		}
		e.log.Warn().Err(err).Str("том", imageID).Int("попытка", attempt).
			Msg("поток imageio остановился — предыдущий билет закрыт, том будет скачан заново")
		select {
		case <-ctx.Done():
			return lastN, ctx.Err()
		case <-time.After(legacyVolumeRetryDelay):
		}
	}
	return lastN, fmt.Errorf("исчерпаны попытки скачивания тома")
}

func (e *Engine) downloadVolumeAttempt(ctx context.Context, client *ovirt.Client, imageID, format, path string,
	onProgress func(int64)) (n int64, retErr error) {

	transfer, err := client.CreateTransferWhenReady(ctx, ovirt.TransferRequest{
		SnapshotID: imageID, Direction: "download", Format: format,
		InactivityTimeout: e.cfg.Transfer.InactivityTimeout,
	}, 10*time.Minute)
	if err != nil {
		return 0, fmt.Errorf("открытие передачи тома: %w", err)
	}
	e.noteTransferOpened(ctx, transfer)
	success := false
	defer func() {
		// Следующий том открывается новой передачей того же диска, а движок
		// 4.3 держит диск, пока прежняя передача не завершилась, — в том
		// числе после успешного finalize. Поэтому завершения ждём всегда.
		wait := legacyTransferCloseWait
		if success {
			wait = legacyTransferFinalizeWait
		}
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wait+time.Minute)
		defer cancel()
		phase := ""
		err := client.CloseTransfer(closeCtx, transfer.ID, success)
		if err == nil {
			phase, err = client.WaitTransferDone(closeCtx, transfer.ID, wait)
		}
		if err != nil {
			e.log.Warn().Err(err).Str("transfer", transfer.ID).Str("фаза", phase).
				Msg("движок не завершил передачу legacy-тома — диск остаётся заблокированным")
			retErr = transferNotReleased(retErr, transfer.ID, phase, err)
			return
		}
		if success && phase != "finished_success" {
			e.log.Warn().Str("transfer", transfer.ID).Str("фаза", phase).
				Msg("передача legacy-тома после finalize завершилась не в finished_success")
		}
	}()
	ready, err := client.WaitTransferReady(ctx, transfer.ID, 10*time.Minute)
	if err != nil {
		return 0, err
	}
	url := ovirt.DataURL(ready, e.cfg.Transfer.PreferProxy)
	urls := []string{url}
	if !e.cfg.Transfer.PreferProxy && ready.ProxyURL != "" && ready.ProxyURL != url {
		urls = append(urls, ready.ProxyURL)
	}
	for _, candidate := range urls {
		var file *os.File
		file, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
		if err != nil {
			return 0, err
		}
		out := &sparseFileWriter{file: file, onProgress: onProgress}
		source := imageio.New(candidate, client.DataHTTPClient()).WithTimeouts(e.imageioTimeouts(0))
		n, err = source.Download(ctx, out)
		if err == nil {
			err = out.finish()
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err == nil || !imageio.IsNetworkError(err) {
			break
		}
	}
	if err != nil {
		return n, err
	}
	success = true
	return n, nil
}

// sparseFileWriter пишет поток в файл, пропуская нулевые блоки: вместо них
// остаются дыры, которые читаются нулями.
type sparseFileWriter struct {
	file       *os.File
	offset     int64
	onProgress func(int64)
	reported   time.Time
}

// sparseBlock — гранулярность дыр; совпадает с -S 4k у qemu-img convert.
const sparseBlock = 4096

func (w *sparseFileWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(len(p), sparseBlock-int(w.offset%sparseBlock))
		block := p[:n]
		if !allZero(block) {
			if _, err := w.file.WriteAt(block, w.offset); err != nil {
				return written, err
			}
		}
		w.offset += int64(n)
		written += n
		p = p[n:]
	}
	if w.onProgress != nil && time.Since(w.reported) > 2*time.Second {
		w.reported = time.Now()
		w.onProgress(w.offset)
	}
	return written, nil
}

// finish задаёт файлу полную длину: хвост из нулей записан дырой.
func (w *sparseFileWriter) finish() error {
	return w.file.Truncate(w.offset)
}

var _ io.Writer = (*sparseFileWriter)(nil)

// copyLegacyChainDisk сохраняет диск, том снапшота которого надо собирать из
// цепочки. В режиме сравнения с основой в репозиторий идут только
// изменившиеся чанки, иначе — все непустые.
func (e *Engine) copyLegacyChainDisk(ctx context.Context, client *ovirt.Client, backend repo.Backend,
	srv *model.Server, vm *model.VM, run *model.BackupRun, req RunRequest, disk ovirt.Disk, index int,
	chunkSize int64, top legacyVolume, formats map[string]string, compare bool) (*DiskManifest, int64, int64, error) {

	if _, err := FindQemuImg(e.cfg.QemuImgPath); err != nil {
		return nil, 0, 0, fmt.Errorf("диск %s: на oVirt без Backup API том qcow2 собирается только через qemu-img — "+
			"установите qemu-img на сервер службы (backup.qemu_img_path): %w", disk.AliasOrName(), err)
	}
	var parent *ChainReader
	if compare && run.ParentRunID != "" {
		reader, err := e.parentDiskReader(ctx, backend, run.ParentRunID, disk.ID)
		if err != nil {
			return nil, 0, 0, err
		}
		parent = reader
		defer parent.Close()
	}

	workDir, err := makeTempWorkspace(e.cfg.TempDir, "jhvirt-legacy-chain-")
	if err != nil {
		return nil, 0, 0, err
	}
	defer os.RemoveAll(workDir)

	total := disk.ProvisionedSize.Int64()
	rawPath, downloaded, err := e.materializeLegacyChain(ctx, client, top, formats, workDir, func(done int64) {
		pct := 0
		if total > 0 {
			pct = int(done * 100 / total)
		}
		_ = e.store.SetRunProgress(ctx, run.ID, minInt(pct, 99), run.ReadBytes+done, run.StoredBytes)
	})
	if err != nil {
		return nil, downloaded, 0, fmt.Errorf("диск %s: сборка образа из томов снапшота: %w", disk.AliasOrName(), err)
	}
	manifest, stored, err := e.writeLegacyDelta(ctx, backend, srv, vm, run, req, disk, index, chunkSize, rawPath, parent)
	if err != nil {
		return nil, downloaded, 0, fmt.Errorf("диск %s: %w", disk.AliasOrName(), err)
	}
	return manifest, downloaded, stored, nil
}
