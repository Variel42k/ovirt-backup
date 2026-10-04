package backup

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"path"
	"strings"
)

// Распознавание образа диска по заголовку.
//
// Образ из другой системы загружается в движок как есть: байты файла пишутся
// в том нового диска без преобразования. Для этого о нём нужно знать три
// вещи до создания диска — формат, виртуальный размер и то, годится ли он
// вообще. Движок после загрузки сверяет виртуальный размер образа с размером
// диска и переводит диск в illegal при расхождении, а узнать размер qcow2
// можно только из его заголовка.

// ImageHeaderBytes — сколько байт начала файла нужно для распознавания.
const ImageHeaderBytes = 4096

// Форматы образов, которые служба загружает в движок как есть.
const (
	ImageFormatQcow2 = "qcow2"
	ImageFormatRaw   = "raw"
)

// ImageInfo describes a disk image file found on a storage.
type ImageInfo struct {
	Path string `json:"path"`
	// Format — qcow2 или raw; для остального — название распознанного формата.
	Format      string `json:"format"`
	FileSize    int64  `json:"file_size"`
	VirtualSize int64  `json:"virtual_size"`
	// QcowVersion — версия формата qcow2 (2 или 3).
	QcowVersion int `json:"qcow_version,omitempty"`
	// Problem — почему образ нельзя загрузить как есть. Пусто — можно.
	Problem string `json:"problem,omitempty"`
	// Notes — на что обратить внимание, не мешающее загрузке.
	Notes []string `json:"notes,omitempty"`
}

// Importable reports whether the image can be uploaded as it is.
func (i ImageInfo) Importable() bool { return i.Problem == "" }

// DiskFormat — формат диска движка под этот образ.
func (i ImageInfo) DiskFormat() string {
	if i.Format == ImageFormatQcow2 {
		return "cow"
	}
	return "raw"
}

var qcow2Magic = []byte{'Q', 'F', 'I', 0xfb}

// foreignImages — форматы, которые узнаются по заголовку, но как есть в движок
// не загружаются. Назвать формат полезнее, чем сказать «не распознан».
var foreignImages = []struct {
	offset int
	magic  []byte
	name   string
}{
	{0, []byte("KDMV"), "VMDK"},
	{0, []byte("# Disk DescriptorFile"), "VMDK (описатель)"},
	{0, []byte("vhdxfile"), "VHDX"},
	{0, []byte("conectix"), "VHD"},
	{0, []byte("<<< Oracle VM VirtualBox Disk Image >>>"), "VDI"},
	{0, []byte("<<< QEMU VM Virtual Disk Image >>>"), "VDI"},
	{0, []byte("VMA\x00"), "архив vzdump (VMA)"},
	{0, []byte{0x1f, 0x8b}, "архив gzip"},
	{0, []byte{0x28, 0xb5, 0x2f, 0xfd}, "архив zstd"},
	{0, []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}, "архив xz"},
	{0, []byte("LZO\x00"), "архив lzo"},
	{257, []byte("ustar"), "архив tar (OVA)"},
}

// InspectImageHeader recognises a disk image by the first bytes of its file.
func InspectImageHeader(name string, header []byte, fileSize int64) ImageInfo {
	info := ImageInfo{Path: name, FileSize: fileSize}
	if fileSize <= 0 {
		info.Format, info.Problem = "неизвестен", "файл пуст"
		return info
	}
	if bytes.HasPrefix(header, qcow2Magic) {
		return inspectQcow2(info, header)
	}
	for _, foreign := range foreignImages {
		end := foreign.offset + len(foreign.magic)
		if len(header) >= end && bytes.Equal(header[foreign.offset:end], foreign.magic) {
			info.Format = foreign.name
			info.Problem = fmt.Sprintf("%s не загружается в движок как есть. Преобразуйте образ в qcow2 "+
				"(qemu-img convert -O qcow2) или распакуйте архив и выберите образ диска внутри него", foreign.name)
			return info
		}
	}

	// Сырой образ заголовка не имеет. Признаётся по расширению или по
	// таблице разделов: иначе «сырым диском» стал бы любой непонятный файл.
	ext := strings.ToLower(path.Ext(name))
	bootable := len(header) >= 512 && header[510] == 0x55 && header[511] == 0xaa
	if ext != ".raw" && ext != ".img" && ext != ".dd" && !bootable {
		info.Format = "неизвестен"
		info.Problem = "формат не распознан: это не qcow2, а на сырой образ диска файл не похож " +
			"(нет таблицы разделов и расширения .raw или .img)"
		return info
	}
	info.Format = ImageFormatRaw
	info.VirtualSize = roundUp(fileSize, 512)
	if !bootable {
		info.Notes = append(info.Notes, "в начале файла нет таблицы разделов: проверьте, что это образ диска целиком, а не раздела")
	}
	return info
}

func inspectQcow2(info ImageInfo, header []byte) ImageInfo {
	info.Format = ImageFormatQcow2
	if len(header) < 72 {
		info.Problem = "заголовок qcow2 прочитан не полностью"
		return info
	}
	be := binary.BigEndian
	info.QcowVersion = int(be.Uint32(header[4:8]))
	backingOffset := be.Uint64(header[8:16])
	info.VirtualSize = int64(be.Uint64(header[24:32]))
	encryption := be.Uint32(header[32:36])

	switch {
	case info.QcowVersion != 2 && info.QcowVersion != 3:
		info.Problem = fmt.Sprintf("версия qcow2 %d не поддерживается (нужна 2 или 3)", info.QcowVersion)
	case info.VirtualSize <= 0:
		info.Problem = "в заголовке qcow2 не указан размер диска"
	case backingOffset != 0:
		info.Problem = "образ ссылается на базовый файл (backing file): это слой, а не диск целиком. " +
			"Соберите его в один образ: qemu-img convert -O qcow2 слой.qcow2 диск.qcow2"
	case encryption != 0:
		info.Problem = "образ зашифрован средствами qcow2: движок такой диск не прочитает"
	}
	if info.Problem != "" {
		return info
	}
	if info.QcowVersion == 3 && len(header) >= 80 {
		incompatible := be.Uint64(header[72:80])
		switch {
		case incompatible&(1<<1) != 0:
			info.Problem = "образ помечен повреждённым (corrupt): исправьте его командой qemu-img check -r all"
		case incompatible&(1<<2) != 0:
			info.Problem = "данные образа лежат в отдельном файле (external data file): одного этого файла недостаточно"
		case incompatible&(1<<0) != 0:
			info.Notes = append(info.Notes, "образ не был закрыт корректно (dirty): он скопирован с работающей ВМ "+
				"или после сбоя. Движок может отвергнуть его при проверке — тогда выполните qemu-img check -r all")
		}
	}
	if info.FileSize > info.VirtualSize+info.VirtualSize/10+(64<<20) {
		info.Notes = append(info.Notes, "файл заметно больше размера диска: в образе есть внутренние снимки "+
			"или много служебных данных")
	}
	return info
}

func roundUp(n, unit int64) int64 {
	if rem := n % unit; rem != 0 {
		return n + unit - rem
	}
	return n
}

// ImageInitialSize — начальный размер тома под образ qcow2 на блочном домене:
// том должен вместить файл образа целиком, расширять его во время загрузки
// некому.
func ImageInitialSize(fileSize int64) int64 {
	const mib = 1 << 20
	return roundUp(fileSize, mib) + 128*mib
}

// ImageDiskLayout — формат и выделение диска под образ, загружаемый как есть.
//
// Формат диска обязан совпадать с форматом файла: в том пишутся байты файла.
// Сырой образ на блочном домене — только диск с полным выделением: тонких
// raw-томов на LVM не бывает.
func ImageDiskLayout(info ImageInfo, storageType string) (format string, sparse bool, initialSize int64) {
	block := IsBlockStorage(storageType)
	if info.Format == ImageFormatQcow2 {
		if block {
			return "cow", true, ImageInitialSize(info.FileSize)
		}
		return "cow", true, 0
	}
	return "raw", !block, 0
}
