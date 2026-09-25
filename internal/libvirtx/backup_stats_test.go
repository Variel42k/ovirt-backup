package libvirtx

import (
	"testing"

	"github.com/digitalocean/go-libvirt"
)

func TestParseStatFree(t *testing.T) {
	free, ok := parseStatFree("262144 4096\n")
	if !ok || free != 1<<30 {
		t.Fatalf("parseStatFree = %d, %v; ожидалось 1 ГиБ", free, ok)
	}
	if _, ok := parseStatFree("stat: cannot read file system information"); ok {
		t.Fatal("непонятный ответ stat не должен разбираться как число")
	}
}

// libvirt отдаёт размер временных файлов бэкапа как unsigned long long; поле
// ищется по имени среди прочей статистики задания.
func TestTypedParamIntFindsScratchUsage(t *testing.T) {
	params := []libvirt.TypedParam{
		{Field: "time_elapsed", Value: libvirt.TypedParamValue{D: 4, I: uint64(1500)}},
		{Field: libvirt.DomainJobDiskTempUsed, Value: libvirt.TypedParamValue{D: 4, I: uint64(3 << 30)}},
	}
	used, ok := typedParamInt(params, libvirt.DomainJobDiskTempUsed)
	if !ok || used != 3<<30 {
		t.Fatalf("typedParamInt = %d, %v; ожидалось 3 ГиБ", used, ok)
	}
	if _, ok := typedParamInt(params, libvirt.DomainJobDiskTempTotal); ok {
		t.Fatal("отсутствующее поле не должно находиться")
	}
	text := []libvirt.TypedParam{{Field: "name", Value: libvirt.TypedParamValue{D: 7, I: "backup"}}}
	if _, ok := typedParamInt(text, "name"); ok {
		t.Fatal("строковое поле не должно читаться как число")
	}
}
