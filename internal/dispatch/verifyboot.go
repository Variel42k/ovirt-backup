package dispatch

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"path"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/kvm"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Тест запуска ВМ: повторно соберите резервную копию, поместите ее на гипервизор, запустите
// ее как изолированную ВМ и дождитесь, пока гость поздоровается.
//
// Проверке требуется соединение libvirt. Она находится здесь, поскольку internal/kvm уже
// зависит от пакета формата. Движок вызывает зарегистрированную функцию, и запись проверки,
// прогресс и отчёт ведутся в одном месте.
//
// Образ передаётся на гипервизор в полном логическом размере со сжатием в сети и
// сохраняется разреженным на диске. Без qemu-guest-agent можно подтвердить только запуск
// ВМ, без ответа гостя.
// Все остальные модальные причины о байтах; Это единственный ответ на вопрос, который на
// самом деле есть у оператора.

// registerVerifiers режимы, которые движок не может выполнять самостоятельно.
func (d *Dispatcher) registerVerifiers() {
	d.Engine.RegisterVerifier(model.VerifyBoot, d.verifyBoot)
}

func (d *Dispatcher) verifyBoot(ctx context.Context, req backup.ExternalVerifyRequest) (resultErr error) {
	ctx, cancel := context.WithCancel(ctx)
	control := &verifyControl{cancel: cancel, done: make(chan struct{})}
	d.activeVerify.Store(req.Record.ID, control)
	defer func() {
		if ctx.Err() != nil {
			resultErr = ctx.Err()
		}
		cancel()
		d.activeVerify.Delete(req.Record.ID)
		close(control.done)
	}()

	if req.Options.TargetID != "" {
		opts, note, err := d.applyVerifyTarget(ctx, req)
		if err != nil {
			return err
		}
		req.Options = opts
		defer func() {
			if req.Report.Boot != nil {
				req.Report.Boot.Notes = append(req.Report.Boot.Notes, note)
			}
		}()
	}
	if req.Options.OnEngine() {
		return d.verifyBootOnEngine(ctx, req)
	}
	set, report, opts := req.Set, req.Report, req.Options

	host, err := d.resolveBootHost(ctx, set.Leaf.ServerID, opts.BootHostID)
	if err != nil {
		return err
	}
	profile, legacy, err := bootProfile(set)
	if err != nil {
		return err
	}
	plans, err := d.planBootDisks(set, opts.DiskID, host.ScratchDir, req.Record.ID, profile)
	if err != nil {
		return err
	}

	log := d.log.With().
		Str("verify", req.Record.ID).Str("backup", set.Leaf.ID).
		Str("хост", host.Name).Int("дисков", len(plans)).Logger()

	conn, err := d.libvirt.ForServer(ctx, host)
	if err != nil {
		return fmt.Errorf("подключение к %s: %w", host.Name, err)
	}

	scratch := host.ScratchDir
	if scratch == "" {
		scratch = "/var/lib/libvirt/qemu"
	}
	driver := kvm.NewDriver(conn, kvm.Config{ScratchDir: scratch}, d.cipher, log)

	if avail, err := driver.AvailableBytes(ctx, scratch); err != nil {
		log.Warn().Err(err).Msg("не удалось узнать свободное место на гипервизоре")
	} else if avail < totalNeeded(plans) {
		return fmt.Errorf(
			"на %s в каталоге %s свободно %s, а для %d образов нужно около %s — "+
				"освободите место или укажите другой каталог в настройках подключения",
			host.Name, scratch, humanBytes(avail), len(plans), humanBytes(totalNeeded(plans)))
	}

	compress := driver.RemoteHasGzip(ctx)
	if !compress {
		log.Info().Msg("на гипервизоре нет gzip — образы передаются без сжатия")
	}

	var uploaded int64
	for i := range plans {
		plan := &plans[i]
		n, uploadErr := d.uploadImage(ctx, driver, plan.Reader, plan.RemotePath,
			compress, req, uploaded, totalLogical(plans))
		plan.Reader.Close()
		plan.Reader = nil
		if uploadErr != nil {
			d.removePlannedImages(driver, plans)
			return fmt.Errorf("передача диска %s: %w", plan.Manifest.Alias, uploadErr)
		}
		uploaded += n
		plan.Report.BytesChecked = n
		report.Disks = append(report.Disks, plan.Report)
	}

	timeout := time.Duration(opts.TimeoutSec) * time.Second
	result, err := driver.RunBootTest(ctx, kvm.BootTest{
		Disks:         bootTestDisks(plans),
		Profile:       profile,
		MemoryMiB:     opts.MemoryMiB,
		VCPUs:         opts.VCPUs,
		Timeout:       timeout,
		KeepOnFailure: opts.KeepOnFailure,
		KeepOnSuccess: keepOnSuccess(opts),
		Name:          set.Leaf.VMName,
		DomainName:    verifyVMName(set.Leaf.VMName, req.Record.ID),
	}, log)
	if err != nil {
		if !opts.KeepOnFailure {
			d.removePlannedImages(driver, plans)
		}
		// Домен никогда не запускался: это неудачная проверка, а не сломанный запрос,
		// поэтому он попадает в отчет.
		for i := range report.Disks {
			report.Disks[i].OK = false
			report.Disks[i].Problems = append(report.Disks[i].Problems, err.Error())
		}
		report.Problems = append(report.Problems, err.Error())
		report.Summary = "проверочную ВМ не удалось запустить"
		report.Boot = &backup.BootReport{Host: host.Name, Notes: []string{err.Error()}}
		return nil
	}

	for i := range report.Disks {
		report.Disks[i].OK = result.Passed()
		if !result.Passed() {
			report.Disks[i].Problems = append(report.Disks[i].Problems, result.Summary())
		}
		report.Disks[i].Problems = append(report.Disks[i].Problems, result.Notes...)
	}
	if !result.Passed() {
		report.Problems = append(report.Problems, result.Summary())
	}
	notes := append([]string{}, result.Notes...)
	if legacy {
		notes = append(notes, "у старого бэкапа нет профиля VM: использован совместимый профиль по данным дисков")
	}
	notes = append(notes, fmt.Sprintf("профиль %s/%s, %s; подключено дисков: %d",
		profile.Architecture, profile.Machine, profile.Firmware, len(plans)))
	report.Summary = fmt.Sprintf("%s (хост %s, %d дисков, %s)",
		result.Summary(), host.Name, len(plans), humanBytes(uploaded))
	report.Boot = &backup.BootReport{
		Host:         host.Name,
		DomainName:   result.DomainName,
		Started:      result.Started,
		AgentReplied: result.AgentReplied,
		Elapsed:      result.Elapsed.Round(time.Second).String(),
		GuestOS:      result.GuestOS,
		Hostname:     result.Hostname,
		ImageBytes:   uploaded,
		Notes:        notes,
		Filesystems:  result.Filesystems,
		Kept:         result.Kept,
	}
	return nil
}

type bootDiskPlan struct {
	DiskID     string
	Manifest   *backup.DiskManifest
	Reader     *backup.ChainReader
	RemotePath string
	Target     string
	Bus        string
	BootOrder  int
	Needed     int64
	Logical    int64
	Report     backup.DiskReport
}

func bootProfile(set *backup.ChainSet) (*backup.VMProfile, bool, error) {
	if set.RunManifestError != nil {
		return nil, false, fmt.Errorf("чтение профиля VM из run.json: %w", set.RunManifestError)
	}
	if set.RunManifest != nil && set.RunManifest.VMProfile != nil {
		copyProfile := *set.RunManifest.VMProfile
		if copyProfile.Version > backup.VMProfileVersion {
			return nil, false, fmt.Errorf("профиль VM версии %d создан более новой версией программы",
				copyProfile.Version)
		}
		copyProfile.Disks = append([]backup.VMProfileDisk(nil), copyProfile.Disks...)
		return &copyProfile, false, nil
	}
	return &backup.VMProfile{
		Version: backup.VMProfileVersion, Source: "legacy", Architecture: "x86_64",
		Machine: "pc", Firmware: "bios", ClockOffset: "utc",
	}, true, nil
}

func (d *Dispatcher) planBootDisks(set *backup.ChainSet, requested, scratch, verifyID string,
	profile *backup.VMProfile) ([]bootDiskPlan, error) {

	ids := append([]string(nil), set.DiskOrder...)
	if requested != "" {
		if _, ok := set.Manifests[requested]; !ok {
			return nil, fmt.Errorf("диска %s нет в этом бэкапе", requested)
		}
		ids = []string{requested}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("в бэкапе нет дисков")
	}
	if scratch == "" {
		scratch = "/var/lib/libvirt/qemu"
	}

	profileByID := make(map[string]backup.VMProfileDisk, len(profile.Disks))
	for _, disk := range profile.Disks {
		profileByID[disk.DiskID] = disk
	}
	usedTargets := map[string]bool{}
	usedBootOrders := map[int]bool{}
	hasBoot := false
	plans := make([]bootDiskPlan, 0, len(ids))
	for i, diskID := range ids {
		chain := set.Manifests[diskID]
		if len(chain) == 0 {
			return nil, fmt.Errorf("для диска %s нет манифеста", diskID)
		}
		manifest := chain[len(chain)-1]
		reader, err := d.Engine.ReaderFor(set, diskID)
		if err != nil {
			for j := range plans {
				plans[j].Reader.Close()
			}
			return nil, err
		}

		attachment := profileByID[diskID]
		bus := backup.NormaliseDiskBus(firstValue(attachment.Bus, manifest.Bus))
		target := firstValue(attachment.Target, manifest.Target)
		if target == "" || usedTargets[target] {
			target = uniqueDiskTarget(bus, i, usedTargets)
		}
		usedTargets[target] = true
		order := attachment.BootOrder
		if order == 0 {
			order = manifest.BootOrder
		}
		if order == 0 && manifest.Bootable {
			order = 1
		}
		if requested != "" {
			order = 1
		}
		if order > 0 {
			for usedBootOrders[order] {
				order++
			}
			usedBootOrders[order] = true
			hasBoot = true
		}

		plans = append(plans, bootDiskPlan{
			DiskID: diskID, Manifest: manifest, Reader: reader,
			RemotePath: path.Join(scratch, fmt.Sprintf("jhv-verify-%s-%02d.raw", verifyID, i)),
			Target:     target, Bus: bus, BootOrder: order,
			Needed:  int64(reader.PresentChunks()) * reader.ChunkSize(),
			Logical: manifest.VirtualSize,
			Report:  backup.DiskReport{DiskID: diskID, Alias: manifest.Alias, OK: true},
		})
	}
	if !hasBoot {
		plans[0].BootOrder = 1
	}
	return plans, nil
}

func firstValue(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func uniqueDiskTarget(bus string, index int, used map[string]bool) string {
	for ; ; index++ {
		candidate := backup.DiskTarget(bus, index)
		if !used[candidate] {
			return candidate
		}
	}
}

func totalNeeded(plans []bootDiskPlan) int64 {
	var total int64
	for _, plan := range plans {
		total += plan.Needed
	}
	return total
}

func totalLogical(plans []bootDiskPlan) int64 {
	var total int64
	for _, plan := range plans {
		total += plan.Logical
	}
	return total
}

func bootTestDisks(plans []bootDiskPlan) []kvm.BootDisk {
	out := make([]kvm.BootDisk, 0, len(plans))
	for _, plan := range plans {
		out = append(out, kvm.BootDisk{
			RemoteImage: plan.RemotePath, Format: "raw", Target: plan.Target,
			Bus: plan.Bus, BootOrder: plan.BootOrder,
		})
	}
	return out
}

func (d *Dispatcher) removePlannedImages(driver *kvm.Driver, plans []bootDiskPlan) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for i := range plans {
		if plans[i].Reader != nil {
			plans[i].Reader.Close()
			plans[i].Reader = nil
		}
		driver.RemoveRemote(cleanupCtx, plans[i].RemotePath)
	}
}

// uploadImage собирает цепь и направляет ее в гипервизор.
//
// Образ восстанавливается и передаётся одновременно через канал. Временный файл на сервере
// копий не создаётся, поэтому терабайтный образ можно проверить при наличии нескольких
// десятков гигабайт.
func (d *Dispatcher) uploadImage(ctx context.Context, driver *kvm.Driver, reader *backup.ChainReader,
	remote string, compress bool, req backup.ExternalVerifyRequest, progressBase, progressTotal int64) (int64, error) {

	pr, pw := io.Pipe()
	var written int64
	lastReport := time.Now()
	started := time.Now()
	lastLogStep := -1

	progress := func(done int64) {
		if progressTotal <= 0 || time.Since(lastReport) < 3*time.Second {
			return
		}
		lastReport = time.Now()
		current := progressBase + done
		var speed int64
		if elapsed := time.Since(started); elapsed > 0 {
			speed = int64(float64(done) / elapsed.Seconds())
		}
		// Загрузка занимает первые 80% бара; Ботинок сам по себе является остальным и не
		// имеет измеримого прогресса.
		pct := int(current * 100 / max64(progressTotal, 1))
		d.Engine.UpdateVerifyTransfer(ctx, req.Record, "writing_data", pct*80/100,
			current, progressTotal, speed)
		if step := pct / 10; step > lastLogStep {
			d.log.Info().Str("verify", req.Record.ID).Int("процент", pct).
				Int64("передано", current).Int64("всего", progressTotal).
				Int64("байт-в-секунду", speed).Msg("образ передаётся на KVM-площадку проверки")
			lastLogStep = step
		}
	}

	go func() {
		var sink io.WriteCloser = pw
		if compress {
			// Выбираем быстрый уровень. Невыделенные диапазоны хорошо сжимаются
			// на любом уровне, а остальные данные обычно уже сжаты внутри гостя.
			gz, err := gzip.NewWriterLevel(pw, gzip.BestSpeed)
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			sink = gz
		}

		n, err := writeImage(ctx, reader, sink, progress)
		written = n
		if err == nil && compress {
			err = sink.Close()
		}
		_ = pw.CloseWithError(err)
	}()

	if err := driver.UploadImage(ctx, pr, remote, kvm.UploadOptions{Compressed: compress}); err != nil {
		_ = pr.CloseWithError(err)
		return 0, err
	}
	d.Engine.UpdateVerifyTransfer(ctx, req.Record, "writing_data", 80,
		progressBase+written, progressTotal, func() int64 {
			if elapsed := time.Since(started); elapsed > 0 {
				return int64(float64(written) / elapsed.Seconds())
			}
			return 0
		}())
	return written, nil
}

// writeImage сериализует слитую цепочку в виде плоского образа.
//
// На принимающей стороне dd пишет последовательно, поэтому передаём нули для сохранения
// смещений последующих данных. conv=sparse не выделяет блоки на диске под эти нули. Пропуск
// нулей при передаче сдвинул бы остаток образа и сорвал проверку загрузки.
func writeImage(ctx context.Context, reader *backup.ChainReader, dst io.Writer, progress func(int64)) (int64, error) {
	var written int64
	zeros := make([]byte, 1<<20)

	err := reader.Stream(ctx, func(ctx context.Context, offset int64, data []byte, zeroLength int64) error {
		if data != nil {
			n, err := dst.Write(data)
			written += int64(n)
			return err
		}
		for zeroLength > 0 {
			n := int64(len(zeros))
			if zeroLength < n {
				n = zeroLength
			}
			w, err := dst.Write(zeros[:n])
			written += int64(w)
			if err != nil {
				return err
			}
			zeroLength -= n
		}
		return nil
	}, progress)

	return written, err
}

// resolveBootHost выбирает гипервизор, на котором запускается тестовая ВМ.
//
// Использовать исходный сервер по умолчанию можно только для libvirt. oVirt не запускает
// образ, которым не управляет, поэтому оператор указывает хост. API проверяет это заранее,
// а здесь то же правило применяется к запускам по расписанию.
func (d *Dispatcher) resolveBootHost(ctx context.Context, ownServerID, requested string) (*model.Server, error) {
	id := requested
	if id == "" {
		own, err := d.store.GetServer(ctx, ownServerID)
		if err != nil {
			return nil, err
		}
		if !own.Kind.UsesLibvirt() {
			return nil, fmt.Errorf(
				"для пробного запуска копии с подключения %q (%s) укажите в задании KVM-хост "+
					"или движок oVirt с кластером и доменом хранения для проверочной ВМ",
				own.Name, own.Kind.Title())
		}
		id = own.ID
	}

	host, err := d.store.GetServer(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("хост для пробного запуска: %w", err)
	}
	if !host.Kind.UsesLibvirt() {
		return nil, fmt.Errorf("подключение %q не является KVM-хостом", host.Name)
	}
	if !host.Enabled {
		return nil, fmt.Errorf("подключение %q отключено", host.Name)
	}
	return host, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
