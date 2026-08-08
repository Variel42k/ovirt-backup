package libvirtx

import (
	"context"
	"encoding/xml"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/digitalocean/go-libvirt"
)

// The pull-mode backup flow, which is what makes a hot backup possible:
//
//  1. DomainBackupBegin freezes a point in time and opens an NBD server on the
//     hypervisor. The guest keeps running and keeps writing; QEMU preserves the
//     old contents of any block the guest overwrites into a scratch file.
//  2. We read from that NBD export at our own pace. base:allocation tells us
//     which blocks hold data; for an incremental run a dirty bitmap tells us
//     which changed since the parent checkpoint.
//  3. DomainAbortJob ends the backup and releases the scratch file.
//
// Step 3 is not optional. A backup left open keeps growing the scratch file
// until the host runs out of space, so it is always issued from a deferred
// call with a context that survives cancellation.

// CheckpointDisk says whether a disk takes part in a checkpoint.
type CheckpointDisk struct {
	// Target — имя устройства в домене (vda).
	Target string
	// Bitmap — заводить ли для него битмап изменённых блоков.
	Bitmap bool
}

// CheckpointSpec describes a checkpoint to establish alongside a backup.
type CheckpointSpec struct {
	Name        string
	Description string
	// Disks перечисляет ВСЕ диски бэкапа, а не только те, для которых нужен
	// битмап. Диск, не упомянутый явно, libvirt пытается снабдить битмапом по
	// умолчанию — а для raw битмап хранить негде, и такой checkpoint отвергается
	// целиком вместе с бэкапом.
	Disks []CheckpointDisk
}

// BitmapDisks returns the disks that will actually get a bitmap.
func (s CheckpointSpec) BitmapDisks() []string {
	var out []string
	for _, disk := range s.Disks {
		if disk.Bitmap {
			out = append(out, disk.Target)
		}
	}
	return out
}

// XML renders the checkpoint description libvirt expects.
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

// BackupDiskSpec describes one disk inside a pull-mode backup.
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

// BackupSpec describes a pull-mode backup.
type BackupSpec struct {
	// SocketPath — unix-сокет, на котором libvirt поднимет NBD-сервер.
	SocketPath string
	// Incremental — имя опорного checkpoint. Пусто — полный бэкап.
	Incremental string
	Disks       []BackupDiskSpec
}

// XML renders the backup description libvirt expects.
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
		// The bitmap is only meaningful when there is a parent to diff against.
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

// CheckpointInfo is a stored checkpoint.
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

// ListCheckpoints returns the domain's checkpoints, oldest first.
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

// HasCheckpoint reports whether a checkpoint still exists. An incremental
// backup whose parent checkpoint is gone cannot be computed, and the only
// correct answer then is to fall back to a full backup.
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

// DeleteCheckpoint removes a checkpoint. Deleting one in the middle of a chain
// makes libvirt merge its bitmap into the child, so the remaining chain stays
// usable — but the backups that referenced it no longer have a base.
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

// BeginBackup starts a pull-mode backup, optionally establishing a checkpoint
// at the same instant so the next run can be incremental.
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

// EndBackup finishes an active backup and releases its scratch files.
//
// libvirt has no dedicated "end backup" call: the backup is a job, and ending
// the job ends the backup. Aborting a pull-mode backup is not a failure — the
// data has already been read by then.
func (c *Conn) EndBackup(ctx context.Context, dom libvirt.Domain) error {
	if err := c.lv.DomainAbortJob(dom); err != nil {
		// "no job" means somebody already ended it, which is the state we want.
		if strings.Contains(strings.ToLower(err.Error()), "no job") {
			return nil
		}
		return fmt.Errorf("завершение бэкапа домена %s: %w", dom.Name, err)
	}
	return nil
}

// BackupInProgress reports whether the domain already has an open backup, which
// is how a leftover from a crashed run is detected at startup.
func (c *Conn) BackupInProgress(ctx context.Context, dom libvirt.Domain) (bool, string) {
	raw, err := c.lv.DomainBackupGetXMLDesc(dom, 0)
	if err != nil {
		return false, ""
	}
	return raw != "", raw
}

// PrepareScratchDir makes sure the directory for scratch files exists and is
// writable by qemu, and reports how much space is free there.
//
// Space matters: the scratch file grows with every block the guest overwrites
// while the backup is being read. A busy database during a slow backup can
// need a surprising amount, and running out aborts the backup.
func (c *Conn) PrepareScratchDir(ctx context.Context, dir string) (freeBytes int64, err error) {
	if dir == "" {
		return 0, fmt.Errorf("не задан каталог для scratch-файлов")
	}
	// qemu runs as its own user, so the directory has to be group-writable by
	// the qemu group rather than owned by the SSH user.
	cmd := fmt.Sprintf("mkdir -p %s && chmod 0771 %s && stat -f -c '%%a %%S' %s",
		shellQuote(dir), shellQuote(dir), shellQuote(dir))
	out, err := c.Run(ctx, cmd)
	if err != nil {
		return 0, fmt.Errorf("подготовка каталога %s на %s: %w", dir, c.cfg.Host, err)
	}

	var blocks, blockSize int64
	if _, scanErr := fmt.Sscanf(strings.TrimSpace(out), "%d %d", &blocks, &blockSize); scanErr != nil {
		// Space is a nice-to-have diagnostic, not a reason to refuse the backup.
		return 0, nil
	}
	return blocks * blockSize, nil
}

// RemoveScratch deletes scratch files left behind by a backup.
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

// RemoveSocket deletes a stale NBD socket file. libvirt refuses to start a
// backup when the socket path already exists.
func (c *Conn) RemoveSocket(ctx context.Context, socketPath string) error {
	if socketPath == "" {
		return nil
	}
	_, err := c.Run(ctx, "rm -f "+shellQuote(socketPath))
	return err
}

// ScratchPath builds a scratch file path for one disk of one run.
func ScratchPath(dir, runID, target string) string {
	return path.Join(dir, fmt.Sprintf("jhv-%s-%s.scratch", shortID(runID), target))
}

// SocketPath builds the NBD socket path for one run.
func SocketPath(dir, runID string) string {
	return path.Join(dir, fmt.Sprintf("jhv-%s.sock", shortID(runID)))
}

// ExportName builds the NBD export name for one disk.
func ExportName(target string) string { return target }

// BitmapName builds the exported dirty bitmap name for one disk.
func BitmapName(target string) string { return "jhv-" + target }

// CheckpointName builds a checkpoint name from a run identifier.
func CheckpointName(runID string) string { return "jhv-" + shortID(runID) }

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
