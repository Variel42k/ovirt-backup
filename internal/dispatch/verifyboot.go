package dispatch

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"path"
	"time"

	"adveng/jh_virt/internal/backup"
	"adveng/jh_virt/internal/kvm"
	"adveng/jh_virt/internal/model"
)

// The boot test: reassemble the backup, put it on a hypervisor, start it as an
// isolated VM and wait for the guest to say hello.
//
// It lives here rather than in internal/backup for the same reason the backup
// driver does — it needs a libvirt connection, and internal/kvm already depends
// on the format package. The engine calls back into it through a registered
// verifier, so the record, its progress and the stored report stay in one place.
//
// The cost is deliberate and worth stating plainly: the image travels to the
// hypervisor at its logical size (compressed on the wire, sparse on disk), and
// a guest without qemu-guest-agent can only ever report "started but silent".
// Every other mode reasons about bytes; this one is the only one that answers
// the question an operator actually has.

// registerVerifiers wires the modes the engine cannot perform on its own.
func (d *Dispatcher) registerVerifiers() {
	d.Engine.RegisterVerifier(model.VerifyBoot, d.verifyBoot)
}

func (d *Dispatcher) verifyBoot(ctx context.Context, req backup.ExternalVerifyRequest) error {
	set, report, opts := req.Set, req.Report, req.Options

	host, err := d.resolveBootHost(ctx, set.Leaf.ServerID, opts.BootHostID)
	if err != nil {
		return err
	}
	diskID, manifest, err := selectBootDisk(set, opts.DiskID)
	if err != nil {
		return err
	}

	log := d.log.With().
		Str("verify", req.Record.ID).Str("backup", set.Leaf.ID).
		Str("хост", host.Name).Str("диск", manifest.Alias).Logger()

	reader, err := d.Engine.ReaderFor(set, diskID)
	if err != nil {
		return err
	}
	defer reader.Close()

	conn, err := d.libvirt.ForServer(ctx, host)
	if err != nil {
		return fmt.Errorf("подключение к %s: %w", host.Name, err)
	}

	scratch := host.ScratchDir
	if scratch == "" {
		scratch = "/var/lib/libvirt/qemu"
	}
	driver := kvm.NewDriver(conn, kvm.Config{ScratchDir: scratch}, d.cipher, log)

	// The sparse write means only the chunks that hold data occupy space, so
	// that — not the disk's logical size — is what has to fit.
	needed := int64(reader.PresentChunks()) * reader.ChunkSize()
	dr := backup.DiskReport{DiskID: diskID, Alias: manifest.Alias, OK: true}

	if avail, err := driver.AvailableBytes(ctx, scratch); err != nil {
		log.Warn().Err(err).Msg("не удалось узнать свободное место на гипервизоре")
	} else if avail < needed {
		return fmt.Errorf(
			"на %s в каталоге %s свободно %s, а для образа нужно около %s — "+
				"освободите место или укажите другой каталог в настройках подключения",
			host.Name, scratch, humanBytes(avail), humanBytes(needed))
	}

	remote := path.Join(scratch, fmt.Sprintf("jhv-verify-%s.raw", req.Record.ID))
	compress := driver.RemoteHasGzip(ctx)
	if !compress {
		log.Info().Msg("на гипервизоре нет gzip — образ передаётся без сжатия")
	}

	uploaded, err := d.uploadImage(ctx, driver, reader, remote, compress, req, needed)
	if err != nil {
		driver.RemoveRemote(context.WithoutCancel(ctx), remote)
		return err
	}
	dr.BytesChecked = uploaded

	timeout := time.Duration(opts.TimeoutSec) * time.Second
	result, err := driver.RunBootTest(ctx, kvm.BootTest{
		RemoteImage:   remote,
		Format:        "raw",
		MemoryMiB:     opts.MemoryMiB,
		VCPUs:         opts.VCPUs,
		Timeout:       timeout,
		KeepOnFailure: opts.KeepOnFailure,
		Name:          set.Leaf.VMName,
	}, log)
	if err != nil {
		// The domain never started: that is a failed verification, not a
		// broken request, so it goes into the report.
		dr.OK = false
		dr.Problems = append(dr.Problems, err.Error())
		report.Disks = append(report.Disks, dr)
		report.Problems = append(report.Problems, err.Error())
		report.Summary = "проверочную ВМ не удалось запустить"
		report.Boot = &backup.BootReport{Host: host.Name, Notes: []string{err.Error()}}
		return nil
	}

	dr.OK = result.Passed()
	if !dr.OK {
		dr.Problems = append(dr.Problems, result.Summary())
		report.Problems = append(report.Problems, result.Summary())
	}
	dr.Problems = append(dr.Problems, result.Notes...)
	report.Disks = append(report.Disks, dr)
	report.Summary = fmt.Sprintf("%s (хост %s, образ %s)",
		result.Summary(), host.Name, humanBytes(uploaded))
	report.Boot = &backup.BootReport{
		Host:         host.Name,
		DomainName:   result.DomainName,
		Started:      result.Started,
		AgentReplied: result.AgentReplied,
		Elapsed:      result.Elapsed.Round(time.Second).String(),
		GuestOS:      result.GuestOS,
		Hostname:     result.Hostname,
		ImageBytes:   uploaded,
		Notes:        result.Notes,
	}
	return nil
}

// uploadImage assembles the chain and streams it to the hypervisor.
//
// The image is produced and consumed at the same time through a pipe: nothing
// is staged on the backup server, which is the only way a terabyte disk can be
// verified on a machine with tens of gigabytes free.
func (d *Dispatcher) uploadImage(ctx context.Context, driver *kvm.Driver, reader *backup.ChainReader,
	remote string, compress bool, req backup.ExternalVerifyRequest, dataBytes int64) (int64, error) {

	pr, pw := io.Pipe()
	var written int64
	lastReport := time.Now()

	progress := func(done int64) {
		if dataBytes <= 0 || time.Since(lastReport) < 3*time.Second {
			return
		}
		lastReport = time.Now()
		// Upload occupies the first 80% of the bar; the boot itself is the
		// rest and has no measurable progress of its own.
		d.Engine.UpdateProgress(ctx, req.Record, int(done*80/max64(dataBytes, 1)))
	}

	go func() {
		var sink io.WriteCloser = pw
		if compress {
			// Fast level, not best: the payload here is mostly holes, which
			// compress to nothing at any level, and real data is usually
			// already compressed inside the guest.
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
	d.Engine.UpdateProgress(ctx, req.Record, 80)
	return written, nil
}

// writeImage serialises the merged chain as a flat image.
//
// Holes are written out as real zeros rather than skipped. dd on the far side
// writes sequentially, so the zeros are what holds later data at its correct
// offset; conv=sparse is what keeps them from reaching the platter. Skipping
// them here instead would shift the whole tail of the image and produce a
// backup that verifies as "did not boot" for a reason that is entirely ours.
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

// resolveBootHost picks the hypervisor the test VM runs on.
//
// Defaulting to the backup's own server is only correct when that server is a
// libvirt host. An oVirt engine cannot start an image it does not manage, so
// there the operator has to name a host — the API refuses the request before it
// gets this far, and this is the guard for the scheduled path.
func (d *Dispatcher) resolveBootHost(ctx context.Context, ownServerID, requested string) (*model.Server, error) {
	id := requested
	if id == "" {
		own, err := d.store.GetServer(ctx, ownServerID)
		if err != nil {
			return nil, err
		}
		if !own.Kind.UsesLibvirt() {
			return nil, fmt.Errorf(
				"пробный запуск требует KVM-хоста: бэкап снят с подключения %q типа %s, "+
					"а движок oVirt не умеет поднимать ВМ из чужого образа — "+
					"запустите проверку вручную и укажите KVM-хост",
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

// selectBootDisk chooses which disk of the backup to boot.
//
// Booting the wrong disk of a multi-disk VM produces a confident "не
// загрузилась" about a data volume that was never bootable, so an ambiguous
// case is refused rather than guessed.
func selectBootDisk(set *backup.ChainSet, requested string) (string, *backup.DiskManifest, error) {
	latest := func(diskID string) *backup.DiskManifest {
		chain := set.Manifests[diskID]
		if len(chain) == 0 {
			return nil
		}
		return chain[len(chain)-1]
	}

	if requested != "" {
		m := latest(requested)
		if m == nil {
			return "", nil, fmt.Errorf("диска %s нет в этом бэкапе", requested)
		}
		return requested, m, nil
	}

	for _, diskID := range set.DiskOrder {
		if m := latest(diskID); m != nil && m.Bootable {
			return diskID, m, nil
		}
	}
	if len(set.DiskOrder) == 1 {
		diskID := set.DiskOrder[0]
		if m := latest(diskID); m != nil {
			return diskID, m, nil
		}
	}
	if len(set.DiskOrder) == 0 {
		return "", nil, fmt.Errorf("в бэкапе нет дисков")
	}
	return "", nil, fmt.Errorf(
		"ни один из %d дисков не помечен загрузочным — укажите диск для пробного запуска явно",
		len(set.DiskOrder))
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
