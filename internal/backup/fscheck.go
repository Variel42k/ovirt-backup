package backup

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
)

// Структурный осмотр восстановленного образа.
//
// Контрольные суммы подтверждают, что байты совпадают с сохранёнными. Даже пустой образ
// проходит такую проверку, если он сохранён без ошибок. Здесь читаем таблицу разделов и
// суперблоки, чтобы определить, выглядит ли образ пригодным для загрузки или монтирования.
//
// Проверяем структуры без монтирования образа. Для оценки его целостности
// открывать файлы не требуется, а монтирование передало бы недоверенный образ ядру.

// Стандартные размеры секторов, опробованные по порядку при поиске таблицы разделов.
var sectorSizes = []int64{512, 4096}

// PartitionScheme именует стиль разделения, найденный на образе.
type PartitionScheme string

const (
	SchemeMBR  PartitionScheme = "mbr"
	SchemeGPT  PartitionScheme = "gpt"
	SchemeNone PartitionScheme = "none"
)

// Partition — одна запись таблицы разделов.
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

// ImageLayout — то, что структурная проверка обнаружила.
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

// Ценности вердикта.
const (
	VerdictUsable     = "usable"
	VerdictSuspicious = "suspicious"
	VerdictEmpty      = "empty"
)

// Usable сообщает, было ли найдено что-то узнаваемое.
func (l *ImageLayout) Usable() bool { return l.Verdict == VerdictUsable }

// Summary отображает однострочное описание для бревен и UI.
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

// InspectImage считывает структуру образа диска.
//
// Он считывает несколько килобайт при нескольких смещениях, поэтому он достаточно дешев,
// чтобы работать на каждой резервной копии, даже когда образ измеряется в терабайтах.
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

	// GPT во-первых: GPT диск также имеет защитный MBR, поэтому проверка MBR сначала
	// неправильно сообщит каждый современный диск как имеющий один огромный раздел.
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

	// Определите файловую систему внутри каждого раздела.
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

// readMBR разбор классической таблицы разделов.
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
		// 0xEE обозначает защитную MBR диска GPT. Действительная таблица находится в другом
		// месте, а эта запись не описывает раздел.
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
	// Таблица, указывающая вне образа, представляет собой таблицу с другого диска.
	for _, p := range parts {
		if p.Start >= size {
			return nil, false
		}
	}
	return parts, true
}

// readGPT разборъ GUID таблицы перегородокъ.
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

	// Проверка здравомыслия перед выделением: поврежденный заголовок не должен превращаться
	// в гигабайтное выделение.
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
			// GPT не имеет "active" флага; Вторую строчку занимает Legacy BIOS.
			Bootable: len(entry) >= 56 && entry[48]&0x04 != 0,
		})
	}
	return parts, len(parts) > 0
}

// probeFilesystem идентифицирует файловую систему по ее магии суперблоков.
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

	// ext2/3/4: магия 0xEF53 в 0x438, метка объема в 0x478.
	if sb := read(0x400, 0x100); sb != nil {
		if binary.LittleEndian.Uint16(sb[0x38:0x3A]) == 0xEF53 {
			featureIncompat := binary.LittleEndian.Uint32(sb[0x60:0x64])
			featureRO := binary.LittleEndian.Uint32(sb[0x64:0x68])
			kind := "ext2"
			if featureIncompat&0x0004 != 0 { // HAS_JOURNAL Живет в компате, но
				kind = "ext3" //             this bit is enough to tell them apart
			}
			if featureIncompat&0x0040 != 0 || featureRO&0x0008 != 0 {
				kind = "ext4"
			}
			return kind, trimNul(sb[0x78:0x88])
		}
	}

	// XFS: "XFSB" по смещению 0, метка по 0x6C.
	if sb := read(0, 0x80); sb != nil && bytes.Equal(sb[0:4], []byte("XFSB")) {
		return "xfs", trimNul(sb[0x6C:0x7C])
	}

	// btrfs: "_BHRfS_M" по смещению 0x10040, метка по 0x102B8 (0x278 от начала суперблока).
	if sb := read(0x10040, 0x8); sb != nil && bytes.Equal(sb, []byte("_BHRfS_M")) {
		label := ""
		if l := read(0x102B8, 0x100); l != nil {
			label = trimNul(l)
		}
		return "btrfs", label
	}

	// NTFS: "NTFS    " по смещению 3.
	if sb := read(0, 0x20); sb != nil && bytes.Equal(sb[3:11], []byte("NTFS    ")) {
		return "ntfs", ""
	}

	// FAT: строка типа сидит в 0x36 (FAT12/16) или 0x52 (FAT32).
	if sb := read(0, 0x60); sb != nil {
		if bytes.HasPrefix(sb[0x36:], []byte("FAT")) {
			return strings.TrimSpace(string(sb[0x36:0x3E])), trimNul(sb[0x2B:0x36])
		}
		if bytes.HasPrefix(sb[0x52:], []byte("FAT32")) {
			return "fat32", trimNul(sb[0x47:0x52])
		}
	}

	// Linux Своп: подпись в конце первой страницы.
	if sb := read(0xFF6, 10); sb != nil && bytes.Equal(sb, []byte("SWAPSPACE2")) {
		return "swap", ""
	}

	// Зашифрованный том LUKS может быть исправен, даже если его содержимое не распознаётся. По
	// заголовку отличаем его от повреждённого образа.
	if sb := read(0, 8); sb != nil && bytes.Equal(sb[0:6], []byte{'L', 'U', 'K', 'S', 0xBA, 0xBE}) {
		return "luks", ""
	}

	// Метка физического тома LVM2 находится в одном из первых четырёх секторов.
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

// formatGUID передаёт GPT тип GUID в его канонической смешанно-эндийской форме.
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
