package libvirtx

import (
	"context"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/digitalocean/go-libvirt"
)

// Поток резервного копирования в режиме тяги, который делает горячее резервное копирование
// возможным:
//
//   1. DomainBackupBegin фиксирует точку и открывает сервер NBD на гипервизоре. Гость
//      продолжает работать, а QEMU сохраняет перезаписываемые старые блоки во временный
//      файл.
//   2. Читаем экспорт NBD. base:allocation показывает выделенные блоки, а карта изменений
//      для инкремента показывает блоки, изменённые после родительского checkpoint.
//   3. DomainAbortJob завершает бэкап и освобождает временный файл.
//
// Шаг 3 обязателен. Пока бэкап открыт, временный файл растёт и может занять весь диск
// хоста. Завершаем бэкап в отложенном вызове с контекстом, который переживает отмену.

// CheckpointDisk говорит, принимает ли диск участие в checkpoint.
type CheckpointDisk struct {
	// Target — имя устройства в домене (vda).
	Target string
	// Bitmap — заводить ли для него битмап изменённых блоков.
	Bitmap bool
}

// CheckpointSpec описывает checkpoint для создания резервной копии.
type CheckpointSpec struct {
	Name        string
	Description string
	// Disks перечисляет ВСЕ диски бэкапа, а не только те, для которых нужен
	// битмап. Диск, не упомянутый явно, libvirt пытается снабдить битмапом по
	// умолчанию — а для raw битмап хранить негде, и такой checkpoint отвергается
	// целиком вместе с бэкапом.
	Disks []CheckpointDisk
}

// BitmapDisks возвращает диски, которые на самом деле получат растровую карту.
func (s CheckpointSpec) BitmapDisks() []string {
	var out []string
	for _, disk := range s.Disks {
		if disk.Bitmap {
			out = append(out, disk.Target)
		}
	}
	return out
}

// XML формирует описание checkpoint в формате libvirt.
func (s CheckpointSpec) XML() (string, error) {
	if s.Name == "" {
		return "", fmt.Errorf("у checkpoint должно быть имя")
	}
	if len(s.BitmapDisks()) == 0 {
		return "", fmt.Errorf("checkpoint без единого битмапа бессмыслен: " +
			"ни один диск не поддерживает отслеживание изменённых блоков")
	}

	var b strings.Builder
	b.WriteString("<domaincheckpoint>\n")
	b.WriteString("  <name>" + escapeXML(s.Name) + "</name>\n")
	if s.Description != "" {
		b.WriteString("  <description>" + escapeXML(s.Description) + "</description>\n")
	}
	b.WriteString("  <disks>\n")
	for _, disk := range s.Disks {
		mode := "no"
		if disk.Bitmap {
			mode = "bitmap"
		}
		b.WriteString(fmt.Sprintf("    <disk name='%s' checkpoint='%s'/>\n",
			escapeXML(disk.Target), mode))
	}
	b.WriteString("  </disks>\n")
	b.WriteString("</domaincheckpoint>\n")
	return b.String(), nil
}

// BackupDiskSpec описывает один диск внутри резервной копии режима вытягивания.
type BackupDiskSpec struct {
	// Target — имя устройства в домене (vda).
	Target string
	// ExportName — под каким именем диск виден на NBD-сервере. Задаём явно,
	// чтобы не угадывать генерируемое libvirt имя.
	ExportName string
	// ExportBitmap — имя битмапа изменённых блоков в NBD. Задаём явно по той
	// же причине; имеет смысл только для инкрементального бэкапа.
	ExportBitmap string
	// ScratchFile — файл, куда QEMU складывает вытесняемые блоки. Должен
	// лежать там, где у qemu есть право записи, и на томе с запасом места.
	ScratchFile string
}

// BackupSpec описывает резервное копирование режима тяги.
type BackupSpec struct {
	// SocketPath — unix-сокет, на котором libvirt поднимет NBD-сервер.
	SocketPath string
	// Incremental — имя опорного checkpoint. Пусто — полный бэкап.
	Incremental string
	Disks       []BackupDiskSpec
}

// XML отображает описание резервной копии libvirt ожидает.
func (s BackupSpec) XML() (string, error) {
	if s.SocketPath == "" {
		return "", fmt.Errorf("не задан путь сокета NBD")
	}
	if len(s.Disks) == 0 {
		return "", fmt.Errorf("не выбран ни один диск")
	}

	var b strings.Builder
	b.WriteString("<domainbackup mode='pull'>\n")
	if s.Incremental != "" {
		b.WriteString("  <incremental>" + escapeXML(s.Incremental) + "</incremental>\n")
	}
	b.WriteString(fmt.Sprintf("  <server transport='unix' socket='%s'/>\n", escapeXML(s.SocketPath)))
	b.WriteString("  <disks>\n")

	for _, disk := range s.Disks {
		if disk.Target == "" || disk.ExportName == "" || disk.ScratchFile == "" {
			return "", fmt.Errorf("диск %q описан не полностью", disk.Target)
		}
		b.WriteString(fmt.Sprintf("    <disk name='%s' backup='yes' type='file' exportname='%s'",
			escapeXML(disk.Target), escapeXML(disk.ExportName)))
		// Растровая карта имеет смысл только тогда, когда есть родитель, против которого
		// нужно дифференцировать.
		if s.Incremental != "" && disk.ExportBitmap != "" {
			b.WriteString(fmt.Sprintf(" exportbitmap='%s'", escapeXML(disk.ExportBitmap)))
		}
		b.WriteString(">\n")
		b.WriteString(fmt.Sprintf("      <scratch file='%s'/>\n", escapeXML(disk.ScratchFile)))
		b.WriteString("    </disk>\n")
	}

	b.WriteString("  </disks>\n")
	b.WriteString("</domainbackup>\n")
	return b.String(), nil
}

// CheckpointInfo — хранимый checkpoint.
type CheckpointInfo struct {
	Name        string
	Description string
	Parent      string
	CreatedAt   time.Time
	// Disks — какие диски покрыты битмапами этого checkpoint.
	Disks []string
}

type checkpointXML struct {
	XMLName      xml.Name `xml:"domaincheckpoint"`
	Name         string   `xml:"name"`
	Description  string   `xml:"description"`
	CreationTime int64    `xml:"creationTime"`
	Parent       struct {
		Name string `xml:"name"`
	} `xml:"parent"`
	Disks struct {
		Disk []struct {
			Name       string `xml:"name,attr"`
			Checkpoint string `xml:"checkpoint,attr"`
			Bitmap     string `xml:"bitmap,attr"`
		} `xml:"disk"`
	} `xml:"disks"`
}

// ListCheckpoints возвращает checkpoint домена от старых к новым.
func (c *Conn) ListCheckpoints(ctx context.Context, dom libvirt.Domain) ([]CheckpointInfo, error) {
	checkpoints, _, err := c.lv.DomainListAllCheckpoints(dom, 1, 0)
	if err != nil {
		return nil, fmt.Errorf("список checkpoint домена %s: %w", dom.Name, err)
	}

	out := make([]CheckpointInfo, 0, len(checkpoints))
	for _, cp := range checkpoints {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, err := c.lv.DomainCheckpointGetXMLDesc(cp, 0)
		if err != nil {
			continue
		}
		var doc checkpointXML
		if err := xml.Unmarshal([]byte(raw), &doc); err != nil {
			continue
		}

		info := CheckpointInfo{
			Name:        doc.Name,
			Description: doc.Description,
			Parent:      doc.Parent.Name,
		}
		if doc.CreationTime > 0 {
			info.CreatedAt = time.Unix(doc.CreationTime, 0).UTC()
		}
		for _, d := range doc.Disks.Disk {
			if d.Checkpoint == "bitmap" {
				info.Disks = append(info.Disks, d.Name)
			}
		}
		out = append(out, info)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// HasCheckpoint сообщает, существует ли checkpoint по-прежнему. Дополнительная резервная
// копия, чья родительская копия checkpoint исчезла, не может быть вычислена, и единственный
// правильный ответ тогда — вернуться к полной резервной копии.
func (c *Conn) HasCheckpoint(ctx context.Context, dom libvirt.Domain, name string) (bool, error) {
	if name == "" {
		return false, nil
	}
	if _, err := c.lv.DomainCheckpointLookupByName(dom, name, 0); err != nil {
		if libvirt.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// DeleteCheckpoint удаляет checkpoint. При удалении из середины цепочки libvirt сливает
// битовую карту с дочерней. Оставшаяся цепочка работоспособна, а бэкапы со ссылкой на
// удалённый checkpoint теряют базу.
func (c *Conn) DeleteCheckpoint(ctx context.Context, dom libvirt.Domain, name string) error {
	cp, err := c.lv.DomainCheckpointLookupByName(dom, name, 0)
	if err != nil {
		if libvirt.IsNotFound(err) {
			return nil
		}
		return err
	}
	return c.lv.DomainCheckpointDelete(cp, 0)
}

// BeginBackup начинает резервное копирование в режиме вытягивания, необязательно
// устанавливая checkpoint в тот же момент, чтобы следующий запуск мог быть инкрементным.
func (c *Conn) BeginBackup(ctx context.Context, dom libvirt.Domain, spec BackupSpec, checkpoint *CheckpointSpec) error {
	backupXML, err := spec.XML()
	if err != nil {
		return err
	}

	var checkpointXML libvirt.OptString
	if checkpoint != nil {
		raw, err := checkpoint.XML()
		if err != nil {
			return err
		}
		checkpointXML = libvirt.OptString{raw}
	}

	if err := c.lv.DomainBackupBegin(dom, backupXML, checkpointXML, 0); err != nil {
		return fmt.Errorf("запуск бэкапа домена %s: %w", dom.Name, err)
	}
	return nil
}

// EndBackup завершает активную резервную копию и выпускает свои временные файлы данных.
//
// В libvirt бэкап реализован заданием и отдельного вызова завершения нет. Остановка
// pull-задания после чтения данных считается штатным завершением.
func (c *Conn) EndBackup(ctx context.Context, dom libvirt.Domain) error {
	if err := c.lv.DomainAbortJob(dom); err != nil {
		// Сообщение "no job" означает, что задание уже завершено.
		if strings.Contains(strings.ToLower(err.Error()), "no job") {
			return nil
		}
		return fmt.Errorf("завершение бэкапа домена %s: %w", dom.Name, err)
	}
	return nil
}

// BackupInProgress сообщает, есть ли у домена уже открытая резервная копия, которая
// определяет, как остаток от аварийного запуска обнаруживается при запуске.
func (c *Conn) BackupInProgress(ctx context.Context, dom libvirt.Domain) (bool, string) {
	raw, err := c.lv.DomainBackupGetXMLDesc(dom, 0)
	if err != nil {
		return false, ""
	}
	return raw != "", raw
}

// CompressionBackupXML сохраняет транспортные ошибки: очистка не должна ошибочно принимать
// недоступный гипервизор за остановленную работу и удалять живые временные файлы данных.
func (c *Conn) CompressionBackupXML(ctx context.Context, dom libvirt.Domain) (string, error) {
	raw, err := c.lv.DomainBackupGetXMLDesc(dom, 0)
	if err != nil {
		message := strings.ToLower(err.Error())
		if strings.Contains(message, "no current backup") || strings.Contains(message, "no backup job") {
			return "", nil
		}
		if state, _, stateErr := c.lv.DomainGetState(dom, 0); stateErr == nil && (stateFromCode(state) == StateShutOff || stateFromCode(state) == StateCrashed) {
			return "", nil
		}
		return "", err
	}
	return raw, nil
}

func (c *Conn) CompressionDomain(ctx context.Context, name, id string) (libvirt.Domain, error) {
	if id == "" {
		dom, _, err := c.LookupDomain(ctx, name)
		return dom, err
	}
	raw, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	if err != nil || len(raw) != 16 {
		return libvirt.Domain{}, fmt.Errorf("некорректный UUID владельца оценки")
	}
	var value libvirt.UUID
	copy(value[:], raw)
	return c.lv.DomainLookupByUUID(value)
}

// Точный XML идентификатор, а не соответствие подстроки, защищает несвязанные задания.
func CompressionBackupOwned(raw, socket string) bool {
	var doc struct {
		Server struct {
			Socket string `xml:"socket,attr"`
		} `xml:"server"`
	}
	return socket != "" && xml.Unmarshal([]byte(raw), &doc) == nil && doc.Server.Socket == socket
}

// PrepareScratchDir убеждается, что каталог для временных файлов данных существует и
// записывается qemu, и сообщает, сколько места там свободно.
//
// Во время чтения бэкапа временный файл растёт с каждым перезаписанным гостем блоком. При
// высокой нагрузке и медленном чтении может понадобиться много места. Когда оно
// заканчивается, бэкап прерывается.
func (c *Conn) PrepareScratchDir(ctx context.Context, dir string) (freeBytes int64, err error) {
	if dir == "" {
		return 0, fmt.Errorf("не задан каталог для scratch-файлов")
	}
	// QEMU работает от отдельного пользователя. Каталог должен быть доступен на запись
	// группе QEMU.
	cmd := fmt.Sprintf("mkdir -p %s && chmod 0771 %s && stat -f -c '%%a %%S' %s",
		shellQuote(dir), shellQuote(dir), shellQuote(dir))
	out, err := c.Run(ctx, cmd)
	if err != nil {
		return 0, fmt.Errorf("подготовка каталога %s на %s: %w", dir, c.cfg.Host, err)
	}
	// Пространство — хорошая диагностика, а не причина отказаться от резервного
	// копирования.
	free, _ := parseStatFree(out)
	return free, nil
}

// ScratchFree возвращает свободное место на томе с рабочим каталогом. Значение измеряется
// во время бэкапа, когда его временный файл и другие файлы на томе могут расти.
func (c *Conn) ScratchFree(ctx context.Context, dir string) (int64, error) {
	out, err := c.Run(ctx, fmt.Sprintf("stat -f -c '%%a %%S' %s", shellQuote(dir)))
	if err != nil {
		return 0, fmt.Errorf("свободное место в %s на %s: %w", dir, c.cfg.Host, err)
	}
	free, ok := parseStatFree(out)
	if !ok {
		return 0, fmt.Errorf("свободное место в %s на %s: непонятный ответ stat: %q", dir, c.cfg.Host, out)
	}
	return free, nil
}

// parseStatFree разбирает «свободные_блоки размер_блока» из stat -f.
func parseStatFree(out string) (int64, bool) {
	var blocks, blockSize int64
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d %d", &blocks, &blockSize); err != nil {
		return 0, false
	}
	return blocks * blockSize, true
}

// BackupScratchUsage возвращает размер временных файлов текущего бэкапа в байтах. ok равно
// false, если libvirt не передал значение, например после завершения задания или на старом
// гипервизоре.
func (c *Conn) BackupScratchUsage(ctx context.Context, dom libvirt.Domain) (used int64, ok bool) {
	_, params, err := c.lv.DomainGetJobStats(dom, 0)
	if err != nil {
		return 0, false
	}
	return typedParamInt(params, libvirt.DomainJobDiskTempUsed)
}

// typedParamInt достаёт целое значение поля из статистики libvirt.
func typedParamInt(params []libvirt.TypedParam, field string) (int64, bool) {
	for _, p := range params {
		if p.Field != field {
			continue
		}
		switch v := p.Value.I.(type) {
		case uint64:
			return int64(v), true
		case int64:
			return v, true
		case uint32:
			return int64(v), true
		case int32:
			return int64(v), true
		}
		return 0, false
	}
	return 0, false
}

// RemoveScratch удаляет временные файлы данных, оставленные резервной копией.
func (c *Conn) RemoveScratch(ctx context.Context, files ...string) error {
	if len(files) == 0 {
		return nil
	}
	quoted := make([]string, 0, len(files))
	for _, f := range files {
		if f == "" {
			continue
		}
		quoted = append(quoted, shellQuote(f))
	}
	if len(quoted) == 0 {
		return nil
	}
	_, err := c.Run(ctx, "rm -f "+strings.Join(quoted, " "))
	return err
}

// RemoveSocket удаляет устаревший NBD файл сокета. libvirt отказывается запускать резервную
// копию, когда путь сокета уже существует.
func (c *Conn) RemoveSocket(ctx context.Context, socketPath string) error {
	if socketPath == "" {
		return nil
	}
	_, err := c.Run(ctx, "rm -f "+shellQuote(socketPath))
	return err
}

// ScratchPath строит временный путь к файлу данных для одного диска из одного запуска.
func ScratchPath(dir, runID, target string) string {
	return path.Join(dir, fmt.Sprintf("jhv-%s-%s.scratch", shortID(runID), target))
}

// SocketPath формирует путь к сокету NBD запуска.
func SocketPath(dir, runID string) string {
	return path.Join(dir, fmt.Sprintf("jhv-%s.sock", shortID(runID)))
}

// ExportName строит NBD имя экспорта для одного диска.
func ExportName(target string) string { return target }

// BitmapName создает экспортированную растровую карту имени измененных блоков для одного
// диска.
func BitmapName(target string) string { return "jhv-" + target }

// checkpointPrefix отличает checkpoint-ы службы от чужих: ротация трогает
// только свои.
const checkpointPrefix = "jhv-"

// CheckpointName строит имя checkpoint из идентификатора запуска.
func CheckpointName(runID string) string { return checkpointPrefix + shortID(runID) }

// IsOwnCheckpoint сообщает, что checkpoint создан бэкапом службы.
func IsOwnCheckpoint(name string) bool { return strings.HasPrefix(name, checkpointPrefix) }

func shortID(id string) string {
	clean := strings.ReplaceAll(id, "-", "")
	if len(clean) > 12 {
		return clean[:12]
	}
	if clean == "" {
		return "run"
	}
	return clean
}

func escapeXML(s string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return s
	}
	return b.String()
}
