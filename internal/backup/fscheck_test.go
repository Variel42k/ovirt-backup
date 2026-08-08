package backup

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"
)

const testImageSize = 64 << 20

// writeExt4 lays down a plausible ext4 superblock at the given partition offset.
func writeExt4(img []byte, partOffset int64, label string, ext4 bool) {
	sb := partOffset + 0x400
	binary.LittleEndian.PutUint16(img[sb+0x38:], 0xEF53) // magic
	if ext4 {
		binary.LittleEndian.PutUint32(img[sb+0x60:], 0x0040) // INCOMPAT_64BIT
	}
	copy(img[sb+0x78:sb+0x88], label)
}

func writeXFS(img []byte, partOffset int64, label string) {
	copy(img[partOffset:], "XFSB")
	copy(img[partOffset+0x6C:partOffset+0x7C], label)
}

func writeSwap(img []byte, partOffset int64) {
	copy(img[partOffset+0xFF6:], "SWAPSPACE2")
}

// writeMBR builds a classic partition table.
type mbrPart struct {
	typeByte byte
	startLBA uint32
	sectors  uint32
	bootable bool
}

func writeMBR(img []byte, parts []mbrPart) {
	img[510], img[511] = 0x55, 0xAA
	for i, p := range parts {
		entry := img[446+i*16 : 446+(i+1)*16]
		if p.bootable {
			entry[0] = 0x80
		}
		entry[4] = p.typeByte
		binary.LittleEndian.PutUint32(entry[8:12], p.startLBA)
		binary.LittleEndian.PutUint32(entry[12:16], p.sectors)
	}
}

// writeGPT builds a GUID partition table with a protective MBR in front of it.
type gptPart struct {
	typeGUID string
	firstLBA uint64
	lastLBA  uint64
	name     string
}

func writeGPT(img []byte, parts []gptPart) {
	const sectorSize = 512
	const entrySize = 128
	const entryLBA = 2

	// Protective MBR, which is what a real GPT disk carries.
	img[510], img[511] = 0x55, 0xAA
	img[446+4] = 0xEE
	binary.LittleEndian.PutUint32(img[446+8:], 1)
	binary.LittleEndian.PutUint32(img[446+12:], 0xFFFFFFFF)

	header := img[sectorSize : sectorSize+92]
	copy(header[0:8], "EFI PART")
	binary.LittleEndian.PutUint64(header[72:80], entryLBA)
	binary.LittleEndian.PutUint32(header[80:84], uint32(len(parts)))
	binary.LittleEndian.PutUint32(header[84:88], entrySize)

	table := img[entryLBA*sectorSize:]
	for i, p := range parts {
		entry := table[i*entrySize : (i+1)*entrySize]
		copy(entry[0:16], parseGUIDForTest(p.typeGUID))
		binary.LittleEndian.PutUint64(entry[32:40], p.firstLBA)
		binary.LittleEndian.PutUint64(entry[40:48], p.lastLBA)

		units := utf16.Encode([]rune(p.name))
		for j, u := range units {
			if 56+j*2+2 > len(entry) {
				break
			}
			binary.LittleEndian.PutUint16(entry[56+j*2:], u)
		}
	}
}

// parseGUIDForTest converts the canonical text form into the mixed-endian
// bytes GPT stores.
func parseGUIDForTest(s string) []byte {
	clean := strings.ReplaceAll(s, "-", "")
	raw := make([]byte, 16)
	for i := 0; i < 16; i++ {
		var b byte
		for j := 0; j < 2; j++ {
			c := clean[i*2+j]
			var v byte
			switch {
			case c >= '0' && c <= '9':
				v = c - '0'
			case c >= 'A' && c <= 'F':
				v = c - 'A' + 10
			case c >= 'a' && c <= 'f':
				v = c - 'a' + 10
			}
			b = b<<4 | v
		}
		raw[i] = b
	}
	out := make([]byte, 16)
	out[0], out[1], out[2], out[3] = raw[3], raw[2], raw[1], raw[0]
	out[4], out[5] = raw[5], raw[4]
	out[6], out[7] = raw[7], raw[6]
	copy(out[8:], raw[8:])
	return out
}

func TestInspectMBRImage(t *testing.T) {
	img := make([]byte, testImageSize)
	const bootStart = 2048
	const rootStart = 4096

	writeMBR(img, []mbrPart{
		{typeByte: 0x83, startLBA: bootStart, sectors: 2048, bootable: true},
		{typeByte: 0x83, startLBA: rootStart, sectors: 100000},
		{typeByte: 0x82, startLBA: 110000, sectors: 4096},
	})
	writeExt4(img, bootStart*512, "boot", false)
	writeExt4(img, rootStart*512, "root", true)
	writeSwap(img, 110000*512)

	layout, err := InspectImage(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("InspectImage: %v", err)
	}

	if layout.Scheme != SchemeMBR {
		t.Fatalf("схема %q, ожидалась mbr", layout.Scheme)
	}
	if len(layout.Partitions) != 3 {
		t.Fatalf("разделов %d, ожидалось 3: %+v", len(layout.Partitions), layout.Partitions)
	}
	if !layout.Partitions[0].Bootable {
		t.Error("первый раздел помечен загрузочным, но флаг не распознан")
	}
	if layout.Partitions[0].Filesystem != "ext2" {
		t.Errorf("ФС первого раздела %q", layout.Partitions[0].Filesystem)
	}
	if layout.Partitions[1].Filesystem != "ext4" || layout.Partitions[1].FSLabel != "root" {
		t.Errorf("второй раздел: %q/%q", layout.Partitions[1].Filesystem, layout.Partitions[1].FSLabel)
	}
	if layout.Partitions[2].Filesystem != "swap" {
		t.Errorf("третий раздел должен быть swap, получено %q", layout.Partitions[2].Filesystem)
	}
	if !layout.Usable() {
		t.Errorf("образ с распознанными ФС должен считаться пригодным: %s", layout.Summary())
	}
}

func TestInspectGPTImage(t *testing.T) {
	img := make([]byte, testImageSize)
	const espStart = 2048
	const rootStart = 6144

	writeGPT(img, []gptPart{
		{typeGUID: "C12A7328-F81F-11D2-BA4B-00A0C93EC93B", firstLBA: espStart, lastLBA: espStart + 2047, name: "EFI"},
		{typeGUID: "4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709", firstLBA: rootStart, lastLBA: 100000, name: "root"},
	})
	writeXFS(img, rootStart*512, "rootfs")

	layout, err := InspectImage(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("InspectImage: %v", err)
	}

	// Защитный MBR не должен принять GPT-диск за один огромный раздел.
	if layout.Scheme != SchemeGPT {
		t.Fatalf("схема %q, ожидалась gpt", layout.Scheme)
	}
	if len(layout.Partitions) != 2 {
		t.Fatalf("разделов %d, ожидалось 2: %+v", len(layout.Partitions), layout.Partitions)
	}
	if layout.Partitions[0].TypeName != "EFI System" {
		t.Errorf("тип первого раздела %q", layout.Partitions[0].TypeName)
	}
	if layout.Partitions[0].Label != "EFI" {
		t.Errorf("метка первого раздела %q", layout.Partitions[0].Label)
	}
	if layout.Partitions[1].Filesystem != "xfs" || layout.Partitions[1].FSLabel != "rootfs" {
		t.Errorf("второй раздел: %q/%q", layout.Partitions[1].Filesystem, layout.Partitions[1].FSLabel)
	}
	if !layout.Usable() {
		t.Errorf("verdict = %q: %s", layout.Verdict, layout.Summary())
	}
}

func TestInspectWholeDiskFilesystem(t *testing.T) {
	// Диск с данными часто размечают файловой системой целиком, без таблицы
	// разделов. Это нормальный образ, а не повреждённый.
	img := make([]byte, testImageSize)
	writeExt4(img, 0, "data", true)

	layout, err := InspectImage(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("InspectImage: %v", err)
	}
	if layout.Scheme != SchemeNone {
		t.Errorf("схема %q, ожидалась none", layout.Scheme)
	}
	if layout.WholeDisk != "ext4" {
		t.Errorf("ФС на всём диске %q", layout.WholeDisk)
	}
	if !layout.Usable() {
		t.Errorf("verdict = %q", layout.Verdict)
	}
	if !strings.Contains(layout.Summary(), "без разметки") {
		t.Errorf("непонятное описание: %s", layout.Summary())
	}
}

func TestInspectEmptyImageIsFlagged(t *testing.T) {
	// Именно этот случай не ловится ни одной контрольной суммой: бэкап
	// пустоты верен побайтово и совершенно бесполезен.
	img := make([]byte, testImageSize)

	layout, err := InspectImage(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("InspectImage: %v", err)
	}
	if layout.Verdict != VerdictEmpty {
		t.Errorf("verdict = %q, ожидался empty", layout.Verdict)
	}
	if layout.Usable() {
		t.Error("пустой образ не должен считаться пригодным")
	}
	if !strings.Contains(layout.Summary(), "пуст") {
		t.Errorf("непонятное описание: %s", layout.Summary())
	}
}

func TestInspectGarbageIsSuspicious(t *testing.T) {
	img := make([]byte, testImageSize)
	for i := range img[:1<<20] {
		img[i] = byte(i%251) + 1
	}

	layout, err := InspectImage(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("InspectImage: %v", err)
	}
	if layout.Verdict != VerdictSuspicious {
		t.Errorf("verdict = %q, ожидался suspicious", layout.Verdict)
	}
	if len(layout.Findings) == 0 {
		t.Error("для подозрительного образа должно быть объяснение")
	}
}

func TestEncryptedVolumeIsRecognised(t *testing.T) {
	// LUKS-том цел, хотя внутри ничего не опознать. Ругаться на него — ложная
	// тревога.
	img := make([]byte, testImageSize)
	copy(img, []byte{'L', 'U', 'K', 'S', 0xBA, 0xBE})

	layout, err := InspectImage(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("InspectImage: %v", err)
	}
	if layout.WholeDisk != "luks" {
		t.Errorf("ФС на всём диске %q, ожидалось luks", layout.WholeDisk)
	}
	if !layout.Usable() {
		t.Error("зашифрованный том должен считаться пригодным")
	}
}

func TestLVMPhysicalVolumeIsRecognised(t *testing.T) {
	img := make([]byte, testImageSize)
	copy(img[512:], "LABELONE")

	layout, err := InspectImage(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("InspectImage: %v", err)
	}
	if layout.WholeDisk != "lvm2-pv" {
		t.Errorf("ФС на всём диске %q, ожидалось lvm2-pv", layout.WholeDisk)
	}
}

func TestPartitionTableFromAnotherDiskIsRejected(t *testing.T) {
	// Таблица, указывающая за пределы образа, пришла не от этого диска —
	// принимать её означало бы рапортовать о разделах, которых нет.
	img := make([]byte, 1<<20)
	writeMBR(img, []mbrPart{
		{typeByte: 0x83, startLBA: 1 << 20, sectors: 1000},
	})

	layout, err := InspectImage(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("InspectImage: %v", err)
	}
	if layout.Scheme != SchemeNone {
		t.Errorf("схема %q: таблица за границами образа должна отбрасываться", layout.Scheme)
	}
}

func TestCorruptedGPTHeaderDoesNotAllocateWildly(t *testing.T) {
	// Испорченный заголовок не должен превращаться в попытку выделить
	// гигабайты под таблицу разделов.
	img := make([]byte, 1<<20)
	header := img[512 : 512+92]
	copy(header[0:8], "EFI PART")
	binary.LittleEndian.PutUint64(header[72:80], 2)
	binary.LittleEndian.PutUint32(header[80:84], 0xFFFFFFFF) // абсурдное число записей
	binary.LittleEndian.PutUint32(header[84:88], 0xFFFFFFFF)

	layout, err := InspectImage(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("InspectImage не должен падать на мусоре: %v", err)
	}
	if layout.Scheme == SchemeGPT {
		t.Error("испорченный заголовок GPT не должен приниматься")
	}
}

func TestGUIDFormatting(t *testing.T) {
	guid := "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"
	if got := formatGUID(parseGUIDForTest(guid)); got != guid {
		t.Errorf("кодирование GUID не обратимо: %s → %s", guid, got)
	}
}
