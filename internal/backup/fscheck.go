package backup

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
)

// Structural inspection of a restored image.
//
// Checksums prove the bytes came back exactly as they were stored. They say
// nothing about whether those bytes were ever a working disk — a backup that
// faithfully preserves an empty image passes every hash check there is. This
// file reads the partition table and the filesystem superblocks the way a
// bootloader would, and reports whether the image looks like something that
// could be booted or mounted.
//
// It deliberately never mounts anything: mounting an untrusted filesystem
// image is a kernel attack surface, and the question here is only "does this
// look intact", not "give me the files".

// Standard sector sizes, tried in order when locating a partition table.
var sectorSizes = []int64{512, 4096}

// PartitionScheme names the partitioning style found on an image.
type PartitionScheme string

const (
	SchemeMBR  PartitionScheme = "mbr"
	SchemeGPT  PartitionScheme = "gpt"
	SchemeNone PartitionScheme = "none"
)

// Partition is one entry of a partition table.
type Partition struct {
	Number     int    `json:"number"`
	Start      int64  `json:"start"`
	Size       int64  `json:"size"`
	TypeID     string `json:"type_id"`
	TypeName   string `json:"type_name"`
	Label      string `json:"label,omitempty"`
	Bootable   bool   `json:"bootable"`
	Filesystem string `json:"filesystem,omitempty"`
	FSLabel    string `json:"fs_label,omitempty"`
}

// ImageLayout is what structural inspection found.
type ImageLayout struct {
	Scheme     PartitionScheme `json:"scheme"`
	SectorSize int64           `json:"sector_size"`
	Partitions []Partition     `json:"partitions"`
	// WholeDisk описывает файловую систему, лежащую прямо на диске без
	// разметки — так часто оформляют диски с данными.
	WholeDisk string `json:"whole_disk_filesystem,omitempty"`
	// Findings — то, что стоит показать оператору.
	Findings []string `json:"findings,omitempty"`
	// Verdict — итог: usable | suspicious | empty.
	Verdict string `json:"verdict"`
}

// Verdict values.
const (
	VerdictUsable     = "usable"
	VerdictSuspicious = "suspicious"
	VerdictEmpty      = "empty"
)

// Usable reports whether something recognisable was found.
func (l *ImageLayout) Usable() bool { return l.Verdict == VerdictUsable }

// Summary renders a one-line description for logs and the UI.
func (l *ImageLayout) Summary() string {
	switch l.Verdict {
	case VerdictEmpty:
		return "образ выглядит пустым: ни таблицы разделов, ни файловой системы не найдено"
	case VerdictSuspicious:
		return "структура образа не распознана — возможно, диск с сырыми данными или шифрованием"
	}
	if l.Scheme == SchemeNone && l.WholeDisk != "" {
		return fmt.Sprintf("файловая система %s на всём диске без разметки", l.WholeDisk)
	}

	var parts []string
	for _, p := range l.Partitions {
		fs := p.Filesystem
		if fs == "" {
			fs = p.TypeName
		}
		parts = append(parts, fmt.Sprintf("%d:%s", p.Number, fs))
	}
	return fmt.Sprintf("разметка %s, разделов %d (%s)",
		strings.ToUpper(string(l.Scheme)), len(l.Partitions), strings.Join(parts, ", "))
}

// InspectImage reads the structure of a disk image.
//
// It reads a few kilobytes at a handful of offsets, so it is cheap enough to
// run on every backup even when the image is measured in terabytes.
func InspectImage(src io.ReaderAt, size int64) (*ImageLayout, error) {
	if size <= 0 {
		return nil, fmt.Errorf("нулевой размер образа")
	}
	layout := &ImageLayout{Scheme: SchemeNone, SectorSize: 512}

	head := make([]byte, 68*1024)
	if int64(len(head)) > size {
		head = head[:size]
	}
	if _, err := src.ReadAt(head, 0); err != nil && err != io.EOF {
		return nil, fmt.Errorf("чтение начала образа: %w", err)
	}

	// GPT first: a GPT disk also carries a protective MBR, so checking MBR
	// first would misreport every modern disk as having one huge partition.
	for _, sectorSize := range sectorSizes {
		if parts, ok := readGPT(src, size, sectorSize); ok {
			layout.Scheme = SchemeGPT
			layout.SectorSize = sectorSize
			layout.Partitions = parts
			break
		}
	}

	if layout.Scheme == SchemeNone {
		if parts, ok := readMBR(head, size); ok {
			layout.Scheme = SchemeMBR
			layout.Partitions = parts
		}
	}

	// Identify the filesystem inside each partition.
	for i := range layout.Partitions {
		p := &layout.Partitions[i]
		fs, label := probeFilesystem(src, p.Start, p.Size)
		p.Filesystem = fs
		p.FSLabel = label
	}

	if layout.Scheme == SchemeNone {
		if fs, label := probeFilesystem(src, 0, size); fs != "" {
			layout.WholeDisk = fs
			if label != "" {
				layout.Findings = append(layout.Findings,
					fmt.Sprintf("метка тома: %s", label))
			}
		}
	}

	classify(layout, head)
	return layout, nil
}

func classify(layout *ImageLayout, head []byte) {
	recognised := 0
	for _, p := range layout.Partitions {
		if p.Filesystem != "" {
			recognised++
		}
	}

	switch {
	case layout.Scheme != SchemeNone && recognised > 0:
		layout.Verdict = VerdictUsable
	case layout.WholeDisk != "":
		layout.Verdict = VerdictUsable
	case layout.Scheme != SchemeNone && recognised == 0:
		layout.Verdict = VerdictSuspicious
		layout.Findings = append(layout.Findings,
			"таблица разделов найдена, но ни в одном разделе не опознана файловая система")
	case allZero(head):
		layout.Verdict = VerdictEmpty
	default:
		layout.Verdict = VerdictSuspicious
		layout.Findings = append(layout.Findings,
			"в начале образа есть данные, но структура не распознана")
	}

	for _, p := range layout.Partitions {
		if p.Size <= 0 {
			layout.Findings = append(layout.Findings,
				fmt.Sprintf("раздел %d имеет нулевой размер", p.Number))
		}
	}
}

func allZero(buf []byte) bool {
	for _, b := range buf {
		if b != 0 {
			return false
		}
	}
	return true
}

// readMBR parses a classic partition table.
func readMBR(head []byte, size int64) ([]Partition, bool) {
	if len(head) < 512 {
		return nil, false
	}
	if head[510] != 0x55 || head[511] != 0xAA {
		return nil, false
	}

	var parts []Partition
	for i := 0; i < 4; i++ {
		entry := head[446+i*16 : 446+(i+1)*16]
		partType := entry[4]
		if partType == 0x00 {
			continue
		}
		// 0xEE marks a protective MBR guarding a GPT disk; the real table is
		// elsewhere and this entry describes nothing.
		if partType == 0xEE {
			return nil, false
		}

		startLBA := int64(binary.LittleEndian.Uint32(entry[8:12]))
		sectors := int64(binary.LittleEndian.Uint32(entry[12:16]))
		if startLBA == 0 || sectors == 0 {
			continue
		}

		parts = append(parts, Partition{
			Number:   i + 1,
			Start:    startLBA * 512,
			Size:     sectors * 512,
			TypeID:   fmt.Sprintf("0x%02X", partType),
			TypeName: mbrTypeName(partType),
			Bootable: entry[0] == 0x80,
		})
	}
	if len(parts) == 0 {
		return nil, false
	}
	// A table pointing outside the image is a table from a different disk.
	for _, p := range parts {
		if p.Start >= size {
			return nil, false
		}
	}
	return parts, true
}

// readGPT parses a GUID partition table.
func readGPT(src io.ReaderAt, size, sectorSize int64) ([]Partition, bool) {
	header := make([]byte, 92)
	if sectorSize >= size {
		return nil, false
	}
	if _, err := src.ReadAt(header, sectorSize); err != nil && err != io.EOF {
		return nil, false
	}
	if !bytes.Equal(header[0:8], []byte("EFI PART")) {
		return nil, false
	}

	entryLBA := int64(binary.LittleEndian.Uint64(header[72:80]))
	entryCount := int64(binary.LittleEndian.Uint32(header[80:84]))
	entrySize := int64(binary.LittleEndian.Uint32(header[84:88]))

	// Sanity-check before allocating: a corrupted header must not turn into a
	// gigabyte allocation.
	if entrySize < 128 || entrySize > 4096 || entryCount <= 0 || entryCount > 1024 {
		return nil, false
	}
	tableOffset := entryLBA * sectorSize
	tableSize := entryCount * entrySize
	if tableOffset <= 0 || tableOffset+tableSize > size {
		return nil, false
	}

	table := make([]byte, tableSize)
	if _, err := src.ReadAt(table, tableOffset); err != nil && err != io.EOF {
		return nil, false
	}

	var parts []Partition
	for i := int64(0); i < entryCount; i++ {
		entry := table[i*entrySize : (i+1)*entrySize]
		typeGUID := entry[0:16]
		if allZero(typeGUID) {
			continue
		}

		firstLBA := int64(binary.LittleEndian.Uint64(entry[32:40]))
		lastLBA := int64(binary.LittleEndian.Uint64(entry[40:48]))
		if lastLBA < firstLBA {
			continue
		}

		guid := formatGUID(typeGUID)
		parts = append(parts, Partition{
			Number:   int(i + 1),
			Start:    firstLBA * sectorSize,
			Size:     (lastLBA - firstLBA + 1) * sectorSize,
			TypeID:   guid,
			TypeName: gptTypeName(guid),
			Label:    decodeUTF16(entry[56:128]),
			// GPT has no "active" flag; bit 2 is the legacy BIOS bootable one.
			Bootable: len(entry) >= 56 && entry[48]&0x04 != 0,
		})
	}
	return parts, len(parts) > 0
}

// probeFilesystem identifies a filesystem by its superblock magic.
func probeFilesystem(src io.ReaderAt, offset, length int64) (name, label string) {
	if length <= 0 {
		return "", ""
	}

	read := func(at, size int64) []byte {
		if at+size > length {
			return nil
		}
		buf := make([]byte, size)
		if _, err := src.ReadAt(buf, offset+at); err != nil && err != io.EOF {
			return nil
		}
		return buf
	}

	// ext2/3/4: magic 0xEF53 at 0x438, volume label at 0x478.
	if sb := read(0x400, 0x100); sb != nil {
		if binary.LittleEndian.Uint16(sb[0x38:0x3A]) == 0xEF53 {
			featureIncompat := binary.LittleEndian.Uint32(sb[0x60:0x64])
			featureRO := binary.LittleEndian.Uint32(sb[0x64:0x68])
			kind := "ext2"
			if featureIncompat&0x0004 != 0 { // HAS_JOURNAL lives in compat, but
				kind = "ext3" //             this bit is enough to tell them apart
			}
			if featureIncompat&0x0040 != 0 || featureRO&0x0008 != 0 {
				kind = "ext4"
			}
			return kind, trimNul(sb[0x78:0x88])
		}
	}

	// XFS: "XFSB" at offset 0, label at 0x6C.
	if sb := read(0, 0x80); sb != nil && bytes.Equal(sb[0:4], []byte("XFSB")) {
		return "xfs", trimNul(sb[0x6C:0x7C])
	}

	// btrfs: "_BHRfS_M" at 0x10040, label at 0x102B8 (0x278 into the superblock).
	if sb := read(0x10040, 0x8); sb != nil && bytes.Equal(sb, []byte("_BHRfS_M")) {
		label := ""
		if l := read(0x102B8, 0x100); l != nil {
			label = trimNul(l)
		}
		return "btrfs", label
	}

	// NTFS: "NTFS    " at offset 3.
	if sb := read(0, 0x20); sb != nil && bytes.Equal(sb[3:11], []byte("NTFS    ")) {
		return "ntfs", ""
	}

	// FAT: the type string sits at 0x36 (FAT12/16) or 0x52 (FAT32).
	if sb := read(0, 0x60); sb != nil {
		if bytes.HasPrefix(sb[0x36:], []byte("FAT")) {
			return strings.TrimSpace(string(sb[0x36:0x3E])), trimNul(sb[0x2B:0x36])
		}
		if bytes.HasPrefix(sb[0x52:], []byte("FAT32")) {
			return "fat32", trimNul(sb[0x47:0x52])
		}
	}

	// Linux swap: signature at the end of the first page.
	if sb := read(0xFF6, 10); sb != nil && bytes.Equal(sb, []byte("SWAPSPACE2")) {
		return "swap", ""
	}

	// LUKS: an encrypted volume is intact even though nothing inside it can be
	// identified, so recognising the header avoids a false alarm.
	if sb := read(0, 8); sb != nil && bytes.Equal(sb[0:6], []byte{'L', 'U', 'K', 'S', 0xBA, 0xBE}) {
		return "luks", ""
	}

	// LVM2 physical volume: the label sits in one of the first four sectors.
	for _, sector := range []int64{0, 512, 1024, 1536} {
		if sb := read(sector, 8); sb != nil && bytes.Equal(sb, []byte("LABELONE")) {
			return "lvm2-pv", ""
		}
	}

	return "", ""
}

func trimNul(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(string(b))
}

func decodeUTF16(b []byte) string {
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u := binary.LittleEndian.Uint16(b[i : i+2])
		if u == 0 {
			break
		}
		units = append(units, u)
	}
	return strings.TrimSpace(string(utf16.Decode(units)))
}

// formatGUID renders a GPT type GUID in its canonical mixed-endian form.
func formatGUID(b []byte) string {
	if len(b) < 16 {
		return ""
	}
	return strings.ToUpper(fmt.Sprintf("%08X-%04X-%04X-%04X-%012X",
		binary.LittleEndian.Uint32(b[0:4]),
		binary.LittleEndian.Uint16(b[4:6]),
		binary.LittleEndian.Uint16(b[6:8]),
		binary.BigEndian.Uint16(b[8:10]),
		b[10:16]))
}

func gptTypeName(guid string) string {
	switch guid {
	case "0FC63DAF-8483-4772-8E79-3D69D8477DE4":
		return "Linux filesystem"
	case "0657FD6D-A4AB-43C4-84E5-0933C84B4F4F":
		return "Linux swap"
	case "E6D6D379-F507-44C2-A23C-238F2A3DF928":
		return "Linux LVM"
	case "4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709":
		return "Linux root (x86-64)"
	case "C12A7328-F81F-11D2-BA4B-00A0C93EC93B":
		return "EFI System"
	case "21686148-6449-6E6F-744E-656564454649":
		return "BIOS boot"
	case "EBD0A0A2-B9E5-4433-87C0-68B6B72699C7":
		return "Microsoft basic data"
	case "DE94BBA4-06D1-4D40-A16A-BFD50179D6AC":
		return "Windows Recovery"
	case "E3C9E316-0B5C-4DB8-817D-F92DF00215AE":
		return "Microsoft reserved"
	default:
		return "unknown"
	}
}

func mbrTypeName(t byte) string {
	switch t {
	case 0x83:
		return "Linux"
	case 0x82:
		return "Linux swap"
	case 0x8E:
		return "Linux LVM"
	case 0x07:
		return "NTFS/exFAT"
	case 0x0B, 0x0C:
		return "FAT32"
	case 0x05, 0x0F:
		return "Extended"
	case 0xEF:
		return "EFI System"
	case 0xFD:
		return "Linux RAID"
	default:
		return "unknown"
	}
}
