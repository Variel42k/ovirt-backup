// Command jvbackup performs hot backups of libvirt/KVM domains and restores
// them, without a database and without the management service running.
//
// It exists for the case the service is built for but cannot serve: the
// management host is gone and somebody needs the data back. Everything it
// needs is in the repository itself.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"adveng/jh_virt/internal/backup"
	"adveng/jh_virt/internal/config"
	"adveng/jh_virt/internal/kvm"
	"adveng/jh_virt/internal/libvirtx"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/repo"
	"adveng/jh_virt/internal/secret"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "backup":
		err = cmdBackup(ctx, os.Args[2:])
	case "list":
		err = cmdList(ctx, os.Args[2:])
	case "verify":
		err = cmdVerify(ctx, os.Args[2:])
	case "restore":
		err = cmdRestore(ctx, os.Args[2:])
	case "inspect":
		err = cmdInspect(ctx, os.Args[2:])
	case "version", "-version", "--version":
		fmt.Printf("jvbackup %s\n", version)
		return
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "неизвестная команда %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "\nошибка: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `jvbackup — горячий бэкап ВМ libvirt/KVM без остановки гостя

Команды:
  backup    снять копию домена
  list      показать копии в хранилище
  verify    проверить копию
  restore   восстановить копию в файл
  inspect   показать, что будет скопировано у домена

Подключение к гипервизору (для backup и inspect):
  -host      адрес гипервизора
  -user      пользователь SSH
  -key       путь к приватному ключу SSH
  -password  пароль SSH (если нет ключа)
  -hostkey   ожидаемый ключ хоста в формате authorized_keys
  -scratch   каталог на гипервизоре под scratch-файлы (по умолчанию /var/lib/libvirt/qemu)

Хранилище (-repo):
  /path/to/dir                     локальный каталог или примонтированная шара
  s3://ключ:секрет@endpoint/bucket S3-совместимое хранилище

Примеры:
  jvbackup backup  -host kvm1 -user root -key ~/.ssh/id_ed25519 -domain db-01 -repo /backup
  jvbackup backup  -host kvm1 -user root -key ~/.ssh/id_ed25519 -domain db-01 -repo /backup -type incremental
  jvbackup list    -repo /backup
  jvbackup verify  -repo /backup -run <id> -mode structure
  jvbackup restore -repo /backup -run <id> -out /var/tmp/restored
`)
}

// hostFlags collects the hypervisor connection options.
type hostFlags struct {
	host     string
	port     int
	user     string
	keyPath  string
	password string
	hostKey  string
	scratch  string
}

func (h *hostFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&h.host, "host", "", "адрес гипервизора")
	fs.IntVar(&h.port, "port", 22, "порт SSH")
	fs.StringVar(&h.user, "user", "root", "пользователь SSH")
	fs.StringVar(&h.keyPath, "key", "", "путь к приватному ключу SSH")
	fs.StringVar(&h.password, "password", "", "пароль SSH")
	fs.StringVar(&h.hostKey, "hostkey", "", "ожидаемый ключ хоста (authorized_keys)")
	fs.StringVar(&h.scratch, "scratch", "/var/lib/libvirt/qemu", "каталог для scratch-файлов на гипервизоре")
}

func (h *hostFlags) connect(ctx context.Context) (*libvirtx.Conn, error) {
	if h.host == "" {
		return nil, fmt.Errorf("не указан -host")
	}

	cfg := libvirtx.Config{
		Host:     h.host,
		Port:     h.port,
		User:     h.user,
		Password: h.password,
		HostKey:  h.hostKey,
	}
	if h.keyPath != "" {
		raw, err := os.ReadFile(h.keyPath)
		if err != nil {
			return nil, fmt.Errorf("чтение ключа %s: %w", h.keyPath, err)
		}
		cfg.PrivateKey = string(raw)
	}
	if cfg.PrivateKey == "" && cfg.Password == "" {
		// Fall back to the usual key locations before giving up.
		for _, candidate := range []string{"id_ed25519", "id_rsa"} {
			home, err := os.UserHomeDir()
			if err != nil {
				break
			}
			path := filepath.Join(home, ".ssh", candidate)
			if raw, err := os.ReadFile(path); err == nil {
				cfg.PrivateKey = string(raw)
				break
			}
		}
	}
	if cfg.HostKey == "" {
		fmt.Fprintln(os.Stderr,
			"предупреждение: ключ хоста не задан (-hostkey), подлинность гипервизора не проверяется")
	}

	return libvirtx.Connect(ctx, cfg)
}

// openRepo turns a -repo value into a storage backend.
func openRepo(ctx context.Context, spec string) (repo.Backend, error) {
	if spec == "" {
		return nil, fmt.Errorf("не указан -repo")
	}

	if strings.HasPrefix(spec, "s3://") {
		target, err := parseS3(spec)
		if err != nil {
			return nil, err
		}
		return repo.Open(ctx, target)
	}

	abs, err := filepath.Abs(spec)
	if err != nil {
		return nil, err
	}
	return repo.Open(ctx, &model.StorageTarget{
		Name: "cli", Kind: model.StorageLocal, BasePath: abs, Enabled: true,
	})
}

// parseS3 accepts s3://accessKey:secretKey@endpoint/bucket[/prefix].
func parseS3(spec string) (*model.StorageTarget, error) {
	rest := strings.TrimPrefix(spec, "s3://")
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return nil, fmt.Errorf("формат: s3://ключ:секрет@endpoint/bucket[/префикс]")
	}
	creds, location := rest[:at], rest[at+1:]

	colon := strings.Index(creds, ":")
	if colon < 0 {
		return nil, fmt.Errorf("в адресе S3 не разделены ключ и секрет")
	}
	parts := strings.SplitN(location, "/", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("в адресе S3 не указан bucket")
	}

	target := &model.StorageTarget{
		Name: "cli", Kind: model.StorageS3, Enabled: true,
		AccessKey: creds[:colon], SecretKey: creds[colon+1:],
		Endpoint: parts[0], Bucket: parts[1],
		UseSSL: true, PathStyle: true,
	}
	if len(parts) == 3 {
		target.Prefix = parts[2]
	}
	return target, nil
}

func newLogger(verbose bool) zerolog.Logger {
	level := zerolog.InfoLevel
	if verbose {
		level = zerolog.DebugLevel
	}
	return zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05"}).
		Level(level).With().Timestamp().Logger()
}

func cmdBackup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	var h hostFlags
	h.register(fs)

	domain := fs.String("domain", "", "имя домена на гипервизоре")
	repoSpec := fs.String("repo", "", "хранилище копий")
	backupType := fs.String("type", "full", "тип: full, incremental, differential")
	quiesce := fs.Bool("quiesce", true, "замораживать файловые системы гостя через гостевого агента")
	encrypt := fs.Bool("encrypt", false, "шифровать данные в хранилище")
	keyFile := fs.String("keyfile", "./jvbackup.key", "файл ключа шифрования")
	verifyFraction := fs.Float64("verify-source", 0,
		"доля чанков для побайтовой сверки с источником: 0 — не сверять, 1 — сверить всё")
	exclude := fs.String("exclude", "", "диски через запятую, которые не копировать (vdb,vdc)")
	compression := fs.String("compression", backup.CompressionZstd,
		"сжатие чанков: "+strings.Join(backup.Compressions, ", ")+
			" (gzip читается любым инструментом, s2 быстрее всех, none — если сжимает хранилище)")
	compressionLevel := fs.Int("compression-level", 3, "уровень сжатия 1..9; для none не используется")
	verbose := fs.Bool("v", false, "подробный вывод")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *domain == "" {
		return fmt.Errorf("не указан -domain")
	}
	// Проверяем до подключения к гипервизору: опечатка в имени алгоритма иначе
	// всплыла бы после снапшота, когда отменять уже дороже.
	if !backup.KnownCompression(*compression) {
		return fmt.Errorf("неизвестное сжатие %q: доступны %s",
			*compression, strings.Join(backup.Compressions, ", "))
	}
	if *compressionLevel < 1 || *compressionLevel > 9 {
		return fmt.Errorf("уровень сжатия должен быть от 1 до 9, получено %d", *compressionLevel)
	}

	log := newLogger(*verbose)

	conn, err := h.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	supported, libvirtVersion, err := conn.SupportsIncrementalBackup(ctx)
	if err != nil {
		return err
	}
	log.Info().Str("гипервизор", h.host).Str("libvirt", libvirtVersion).Msg("подключено")
	if !supported {
		return fmt.Errorf("libvirt %s не поддерживает pull-режим бэкапа (нужен 6.0+)", libvirtVersion)
	}

	backend, err := openRepo(ctx, *repoSpec)
	if err != nil {
		return err
	}
	defer backend.Close()

	var cipher *secret.Cipher
	if *encrypt {
		cipher, err = secret.NewFromConfig(config.SecretsConfig{KeyFile: *keyFile})
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr,
			"ключ шифрования: %s — без него копии не расшифровать, храните его отдельно от бэкапов\n", *keyFile)
	}

	// Without a database, the parent for an incremental comes from the
	// repository itself.
	runs, err := backup.ScanRepository(ctx, backend, "")
	if err != nil {
		return err
	}

	requested := model.BackupType(*backupType)
	req := kvm.Request{
		DomainName:           *domain,
		Type:                 requested,
		RunID:                uuid.NewString(),
		Backend:              backend,
		ServerID:             h.host,
		Quiesce:              *quiesce,
		Encrypt:              *encrypt,
		SourceVerifyFraction: *verifyFraction,
	}
	if *exclude != "" {
		req.ExcludeDisks = strings.Split(*exclude, ",")
	}

	if requested.NeedsParent() {
		parent, ok := backup.LatestUsable(runs, *domain, requested == model.BackupDifferential)
		if ok {
			req.ParentCheckpoint = parent.Manifest.ToCheckpointID
			req.ParentRunID = parent.Manifest.RunID
			req.ChainID = parent.Manifest.ChainID
			req.ChainIndex = parent.Manifest.ChainIndex + 1
			log.Info().Str("опора", parent.Manifest.RunID).
				Str("checkpoint", req.ParentCheckpoint).Msg("найдена предыдущая точка")
		} else {
			log.Info().Msg("предыдущей точки в хранилище нет — будет полный бэкап")
		}
	}
	if req.ChainID == "" {
		req.ChainID = req.RunID
	}

	started := time.Now()
	req.RepoPath = repo.RunPrefix(h.host, *domain, *domain, started.UTC(), req.RunID)
	req.OnProgress = func(target string, done, total int64) {
		if total > 0 {
			fmt.Fprintf(os.Stderr, "\r%s: %d%% (%s из %s)   ",
				target, done*100/total, humanBytes(done), humanBytes(total))
		}
	}

	driver := kvm.NewDriver(conn, kvm.Config{
		ScratchDir:       h.scratch,
		Compression:      *compression,
		CompressionLevel: *compressionLevel,
	}, cipher, log)
	result, err := driver.Backup(ctx, req)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}

	doc := &backup.RunManifest{
		RunID:            req.RunID,
		ChainID:          req.ChainID,
		ParentRunID:      req.ParentRunID,
		ChainIndex:       req.ChainIndex,
		Type:             result.Type,
		ServerName:       h.host,
		VMID:             *domain,
		VMName:           *domain,
		FromCheckpointID: result.ParentCheckpoint,
		ToCheckpointID:   result.Checkpoint,
		CreatedAt:        started.UTC(),
		EndedAt:          time.Now().UTC(),
		Encrypted:        *encrypt,
		LogicalBytes:     result.ReadBytes,
		StoredBytes:      result.StoredBytes,
	}
	for _, m := range result.Manifests {
		doc.Compression = m.Compression
		doc.Disks = append(doc.Disks, backup.RunManifestDisk{
			DiskID:      m.DiskID,
			Alias:       m.Alias,
			Index:       m.Index,
			VirtualSize: m.VirtualSize,
			ManifestKey: repo.DiskManifestKey(req.RepoPath, m.Index, m.DiskID),
			DataKey:     m.DataKey,
			ChunkCount:  m.ChunkCount(),
			StoredBytes: m.StoredBytes,
			DataSHA256:  m.DataSHA256,
		})
	}
	if err := backup.WriteRunManifest(ctx, backend, req.RepoPath, doc); err != nil {
		return fmt.Errorf("запись манифеста запуска: %w", err)
	}

	fmt.Printf("\nБэкап выполнен\n")
	fmt.Printf("  идентификатор : %s\n", req.RunID)
	fmt.Printf("  тип           : %s\n", result.Type.Title())
	if result.Note != "" {
		fmt.Printf("  примечание    : %s\n", result.Note)
	}
	fmt.Printf("  дисков        : %d\n", len(result.Manifests))
	fmt.Printf("  прочитано     : %s\n", humanBytes(result.ReadBytes))
	fmt.Printf("  записано      : %s\n", humanBytes(result.StoredBytes))
	fmt.Printf("  checkpoint    : %s\n", result.Checkpoint)
	fmt.Printf("  длительность  : %s\n", time.Since(started).Round(time.Second))
	if result.SourceChecked > 0 {
		fmt.Printf("  сверка с источником: %d чанков, расхождений %d\n",
			result.SourceChecked, result.SourceMismatch)
	}
	for target, reason := range result.SkippedDisks {
		fmt.Printf("  пропущен %s: %s\n", target, reason)
	}
	return nil
}

func cmdList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	repoSpec := fs.String("repo", "", "хранилище копий")
	vm := fs.String("domain", "", "показать только этот домен")
	if err := fs.Parse(args); err != nil {
		return err
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
	if len(runs) == 0 {
		fmt.Println("в хранилище нет завершённых копий")
		return nil
	}

	sort.Slice(runs, func(i, j int) bool {
		return runs[i].Manifest.CreatedAt.After(runs[j].Manifest.CreatedAt)
	})

	fmt.Printf("%-38s %-16s %-14s %-20s %10s  %s\n",
		"ИДЕНТИФИКАТОР", "ДОМЕН", "ТИП", "СОЗДАН", "РАЗМЕР", "ЦЕПОЧКА")
	for _, r := range runs {
		m := r.Manifest
		if *vm != "" && m.VMName != *vm {
			continue
		}
		chain := "полный"
		if m.ParentRunID != "" {
			chain = fmt.Sprintf("звено %d ← %s", m.ChainIndex, short(m.ParentRunID))
		}
		if _, err := backup.FindChain(runs, m.RunID); err != nil {
			chain = "ЦЕПОЧКА НЕПОЛНА"
		}
		fmt.Printf("%-38s %-16s %-14s %-20s %10s  %s\n",
			m.RunID, truncate(m.VMName, 16), string(m.Type),
			m.CreatedAt.Local().Format("2006-01-02 15:04:05"),
			humanBytes(m.StoredBytes), chain)
	}
	return nil
}

func cmdInspect(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	var h hostFlags
	h.register(fs)
	domain := fs.String("domain", "", "имя домена")
	if err := fs.Parse(args); err != nil {
		return err
	}

	conn, err := h.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if *domain == "" {
		domains, err := conn.ListDomains(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("%-24s %-16s %6s %8s %s\n", "ДОМЕН", "СОСТОЯНИЕ", "vCPU", "ПАМЯТЬ", "ДИСКОВ")
		for _, d := range domains {
			fmt.Printf("%-24s %-16s %6d %8s %d\n",
				truncate(d.Name, 24), d.State.Title(), d.VCPUs,
				humanBytes(d.MemoryKiB*1024), len(d.BackupDisks()))
		}
		return nil
	}

	_, info, err := conn.LookupDomain(ctx, *domain)
	if err != nil {
		return err
	}

	fmt.Printf("Домен %s\n", info.Name)
	fmt.Printf("  состояние      : %s\n", info.State.Title())
	fmt.Printf("  vCPU / память  : %d / %s\n", info.VCPUs, humanBytes(info.MemoryKiB*1024))
	fmt.Printf("  гостевой агент : %s\n", yesNo(info.GuestAgent))

	ready, blockers := info.CBTReady()
	fmt.Printf("  инкременты     : %s\n", yesNo(ready))
	if !ready {
		fmt.Printf("    мешают диски : %s\n", strings.Join(blockers, ", "))
	}

	fmt.Println("\n  Диски:")
	for _, disk := range info.Disks {
		if disk.BackupCandidate() {
			fmt.Printf("    %-6s %-8s %-8s копируется%s\n",
				disk.Target, disk.Format, disk.Bus,
				map[bool]string{true: "", false: " (без CBT)"}[disk.SupportsCBT()])
			continue
		}
		fmt.Printf("    %-6s %-8s %-8s пропускается: %s\n",
			disk.Target, disk.Format, disk.Bus, disk.SkipReason())
	}
	return nil
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func yesNo(v bool) string {
	if v {
		return "да"
	}
	return "нет"
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d Б", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %sБ", float64(n)/float64(div), []string{"К", "М", "Г", "Т", "П"}[exp])
}
