package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/repo"
	"github.com/Variel42k/ovirt-backup/internal/secret"
)

// Verification and restore work entirely from the repository. Nothing here
// consults a database, because the situation this command exists for is the
// one where there is no database left to consult.

func cmdVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	timezone := timezoneFlag(fs)
	repoSpec := fs.String("repo", "", "хранилище копий")
	runID := fs.String("run", "", "идентификатор копии")
	mode := fs.String("mode", "manifest", "режим: quick, chain, manifest, structure, restore")
	keyFile := fs.String("keyfile", "./jvbackup.key", "файл ключа шифрования")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := applyTimezone(*timezone); err != nil {
		return err
	}
	if *runID == "" {
		return fmt.Errorf("не указан -run")
	}

	backend, err := openRepo(ctx, *repoSpec)
	if err != nil {
		return err
	}
	defer backend.Close()

	runs, err := backup.ScanRepository(ctx, backend, "")
	if err != nil {
		return err
	}
	chain, err := backup.FindChain(runs, *runID)
	if err != nil {
		return err
	}
	fmt.Printf("Цепочка из %d звеньев:\n", len(chain))
	for _, link := range chain {
		fmt.Printf("  %-38s %-14s %s\n", link.Manifest.RunID, string(link.Manifest.Type),
			link.Manifest.CreatedAt.In(cliLocation).Format("2006-01-02 15:04:05"))
	}

	cipher := loadCipherIfPresent(*keyFile)
	byDisk, order, err := backup.LoadChainManifests(ctx, backend, chain)
	if err != nil {
		return err
	}

	started := time.Now()
	problems := 0

	for _, diskID := range order {
		manifests := byDisk[diskID]
		alias := manifests[len(manifests)-1].Alias
		fmt.Printf("\nДиск %s (%s):\n", diskID, alias)

		switch *mode {
		case "quick":
			problems += verifyQuick(ctx, backend, manifests)
		case "chain":
			problems += verifyChain(manifests)
		case "manifest":
			problems += verifyManifest(ctx, backend, cipher, manifests)
		case "structure":
			problems += verifyStructure(ctx, backend, cipher, manifests)
		case "restore":
			problems += verifyRestore(ctx, backend, cipher, manifests)
		default:
			return fmt.Errorf("неизвестный режим %q", *mode)
		}
	}

	fmt.Printf("\nПроверка заняла %s\n", time.Since(started).Round(time.Second))
	if problems > 0 {
		return fmt.Errorf("проверка не пройдена: найдено проблем — %d", problems)
	}
	fmt.Println("Проверка пройдена.")
	return nil
}

func verifyQuick(ctx context.Context, backend repo.Backend, manifests []*backup.DiskManifest) int {
	problems := 0
	for _, m := range manifests {
		info, err := backend.Stat(ctx, m.DataKey)
		if err != nil {
			fmt.Printf("  ✗ объект %s недоступен: %v\n", m.DataKey, err)
			problems++
			continue
		}
		if info.Size != m.StoredBytes {
			fmt.Printf("  ✗ размер %s: %d вместо %d\n", m.DataKey, info.Size, m.StoredBytes)
			problems++
			continue
		}
		fmt.Printf("  ✓ %s: %s на месте\n", m.RunID, humanBytes(info.Size))
	}
	return problems
}

func verifyChain(manifests []*backup.DiskManifest) int {
	problems := 0
	for i, m := range manifests {
		if i == 0 {
			continue
		}
		prev := manifests[i-1]
		if m.ParentRunID != prev.RunID {
			fmt.Printf("  ✗ %s ссылается на родителя %s, а перед ним %s\n",
				m.RunID, m.ParentRunID, prev.RunID)
			problems++
		}
		// The increment was computed against the parent's checkpoint; a break
		// here means the delta was taken from somewhere else entirely.
		if m.FromCheckpointID != "" && prev.ToCheckpointID != "" &&
			m.FromCheckpointID != prev.ToCheckpointID {
			fmt.Printf("  ✗ %s посчитан от checkpoint %s, а родитель закончился на %s\n",
				m.RunID, m.FromCheckpointID, prev.ToCheckpointID)
			problems++
		}
	}
	if problems == 0 {
		fmt.Printf("  ✓ цепочка из %d звеньев согласована\n", len(manifests))
	}
	return problems
}

func verifyManifest(ctx context.Context, backend repo.Backend, cipher *secret.Cipher,
	manifests []*backup.DiskManifest) int {
	problems := 0

	for _, m := range manifests {
		if err := backup.VerifyDataObject(ctx, backend, m); err != nil {
			fmt.Printf("  ✗ %s: %v\n", m.RunID, err)
			problems++
			continue
		}

		reader, err := backup.NewChainReader(backend, cipher, []*backup.DiskManifest{m})
		if err != nil {
			fmt.Printf("  ✗ %s: %v\n", m.RunID, err)
			problems++
			continue
		}

		bad := 0
		for _, chunk := range m.Chunks {
			if _, err := reader.ReadChunk(ctx, chunk.Index); err != nil {
				if bad < 5 {
					fmt.Printf("  ✗ чанк %d: %v\n", chunk.Index, err)
				}
				bad++
			}
		}
		reader.Close()

		if bad > 0 {
			fmt.Printf("  ✗ %s: повреждено чанков — %d из %d\n", m.RunID, bad, len(m.Chunks))
			problems += bad
			continue
		}
		fmt.Printf("  ✓ %s: %d чанков, контрольные суммы совпали\n", m.RunID, len(m.Chunks))
	}
	return problems
}

func verifyStructure(ctx context.Context, backend repo.Backend, cipher *secret.Cipher,
	manifests []*backup.DiskManifest) int {

	reader, err := backup.NewChainReader(backend, cipher, manifests)
	if err != nil {
		fmt.Printf("  ✗ %v\n", err)
		return 1
	}
	defer reader.Close()

	layout, err := backup.InspectImage(reader.ReaderAt(ctx), reader.VirtualSize())
	if err != nil {
		fmt.Printf("  ✗ %v\n", err)
		return 1
	}

	fmt.Printf("  %s\n", layout.Summary())
	for _, p := range layout.Partitions {
		fs := p.Filesystem
		if fs == "" {
			fs = "не опознана"
		}
		label := p.FSLabel
		if label == "" {
			label = p.Label
		}
		fmt.Printf("    раздел %d: %-10s %-12s %s %s\n",
			p.Number, humanBytes(p.Size), p.TypeName, fs, label)
	}
	for _, note := range layout.Findings {
		fmt.Printf("    ! %s\n", note)
	}

	if layout.Verdict == backup.VerdictEmpty {
		fmt.Println("  ✗ образ пуст — контрольные суммы этого не покажут, а восстанавливать нечего")
		return 1
	}
	fmt.Println("  ✓ структура образа распознана")
	return 0
}

func verifyRestore(ctx context.Context, backend repo.Backend, cipher *secret.Cipher,
	manifests []*backup.DiskManifest) int {

	reader, err := backup.NewChainReader(backend, cipher, manifests)
	if err != nil {
		fmt.Printf("  ✗ %v\n", err)
		return 1
	}
	defer reader.Close()

	tmp, err := os.CreateTemp("", "jvbackup-verify-*.raw")
	if err != nil {
		fmt.Printf("  ✗ %v\n", err)
		return 1
	}
	path := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(path)
	}()

	if err := tmp.Truncate(reader.VirtualSize()); err != nil {
		fmt.Printf("  ✗ резервирование размера: %v\n", err)
		return 1
	}

	var written int64
	err = reader.Stream(ctx, func(ctx context.Context, offset int64, data []byte, zeroLength int64) error {
		if data == nil {
			return nil // файл уже разрежен и нулевой
		}
		if _, err := tmp.WriteAt(data, offset); err != nil {
			return err
		}
		written += int64(len(data))
		return nil
	}, nil)
	if err != nil {
		fmt.Printf("  ✗ сборка образа: %v\n", err)
		return 1
	}

	fmt.Printf("  ✓ образ собран целиком: %s данных из %s логического объёма\n",
		humanBytes(written), humanBytes(reader.VirtualSize()))
	return 0
}

func cmdRestore(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	timezone := timezoneFlag(fs)
	repoSpec := fs.String("repo", "", "хранилище копий")
	runID := fs.String("run", "", "идентификатор копии")
	outDir := fs.String("out", ".", "каталог для восстановленных образов")
	format := fs.String("format", "raw", "формат: raw или qcow2 (qcow2 требует qemu-img)")
	only := fs.String("disk", "", "восстановить только этот диск")
	keyFile := fs.String("keyfile", "./jvbackup.key", "файл ключа шифрования")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := applyTimezone(*timezone); err != nil {
		return err
	}
	if *runID == "" {
		return fmt.Errorf("не указан -run")
	}

	backend, err := openRepo(ctx, *repoSpec)
	if err != nil {
		return err
	}
	defer backend.Close()

	runs, err := backup.ScanRepository(ctx, backend, "")
	if err != nil {
		return err
	}
	chain, err := backup.FindChain(runs, *runID)
	if err != nil {
		return err
	}
	leaf := chain[len(chain)-1].Manifest

	cipher := loadCipherIfPresent(*keyFile)
	byDisk, order, err := backup.LoadChainManifests(ctx, backend, chain)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(*outDir, 0o750); err != nil {
		return err
	}

	fmt.Printf("Восстановление копии %s (%s от %s), звеньев в цепочке: %d\n",
		leaf.RunID, leaf.VMName, leaf.CreatedAt.In(cliLocation).Format("2006-01-02 15:04:05"), len(chain))

	for _, diskID := range order {
		if *only != "" && diskID != *only {
			continue
		}
		manifests := byDisk[diskID]
		alias := manifests[len(manifests)-1].Alias

		reader, err := backup.NewChainReader(backend, cipher, manifests)
		if err != nil {
			return err
		}

		name := fmt.Sprintf("%s_%s_%s.raw",
			repo.Segment(leaf.VMName), repo.Segment(alias), leaf.CreatedAt.Format("20060102-150405"))
		rawPath := filepath.Join(*outDir, name)

		file, err := os.OpenFile(rawPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
		if err != nil {
			reader.Close()
			return err
		}
		// Setting the size up front makes the file sparse, so zero regions
		// cost neither space nor writes.
		if err := file.Truncate(reader.VirtualSize()); err != nil {
			file.Close()
			reader.Close()
			return err
		}

		started := time.Now()
		total := reader.VirtualSize()
		err = reader.Stream(ctx, func(ctx context.Context, offset int64, data []byte, zeroLength int64) error {
			if data == nil {
				return nil
			}
			_, err := file.WriteAt(data, offset)
			return err
		}, func(done int64) {
			if total > 0 {
				fmt.Fprintf(os.Stderr, "\r  %s: %d%%   ", alias, done*100/total)
			}
		})
		fmt.Fprintln(os.Stderr)

		syncErr := file.Sync()
		closeErr := file.Close()
		reader.Close()

		if err != nil {
			return fmt.Errorf("диск %s: %w", diskID, err)
		}
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}

		final := rawPath
		if *format == "qcow2" {
			qcowPath := rawPath[:len(rawPath)-len(".raw")] + ".qcow2"
			if err := backup.ConvertToQcow2(ctx, "", rawPath, qcowPath); err != nil {
				return fmt.Errorf("конвертация в qcow2: %w", err)
			}
			_ = os.Remove(rawPath)
			final = qcowPath
		}

		fmt.Printf("  ✓ %s → %s (%s, за %s)\n",
			alias, final, humanBytes(total), time.Since(started).Round(time.Second))
	}
	return nil
}

// loadCipherIfPresent opens the encryption key when the file exists. A
// repository holding unencrypted backups needs no key, and demanding one would
// make the common case harder for no reason.
func loadCipherIfPresent(keyFile string) *secret.Cipher {
	if _, err := os.Stat(keyFile); err != nil {
		return nil
	}
	cipher, err := secret.NewFromConfig(config.SecretsConfig{KeyFile: keyFile})
	if err != nil {
		fmt.Fprintf(os.Stderr, "предупреждение: ключ %s не загружен: %v\n", keyFile, err)
		return nil
	}
	return cipher
}
