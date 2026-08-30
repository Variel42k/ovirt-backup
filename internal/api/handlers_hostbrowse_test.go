package api

import "testing"

// Проверка границы корня на удалённой машине делается сравнением строк: сам
// путь разрешает гипервизор, а сюда приходит уже разрешённый. Классическая
// ошибка такого сравнения — совпадение по префиксу с чужим каталогом, у
// которого имя начинается так же.
func TestRemoteRootBoundary(t *testing.T) {
	cases := []struct {
		root, candidate string
		inside          bool
		why             string
	}{
		{"/var/lib/libvirt", "/var/lib/libvirt", true, "сам корень"},
		{"/var/lib/libvirt", "/var/lib/libvirt/qemu", true, "вложенный каталог"},
		{"/var/lib/libvirt", "/var/lib/libvirt/qemu/scratch", true, "глубже одного уровня"},
		{"/var/lib/libvirt", "/var/lib/libvirt-evil", false,
			"каталог-сосед с тем же началом имени не внутри корня"},
		{"/var/lib/libvirt", "/var/lib", false, "родитель не внутри корня"},
		{"/var/lib/libvirt", "/etc", false, "куда мог привести symlink на хосте"},
		{"/var/lib/libvirt/", "/var/lib/libvirt/qemu", true,
			"завершающая косая черта в корне ничего не меняет"},
	}

	for _, c := range cases {
		if got := withinRemoteRoot(c.root, c.candidate); got != c.inside {
			t.Errorf("%s: withinRemoteRoot(%q, %q) = %v, ожидалось %v",
				c.why, c.root, c.candidate, got, c.inside)
		}
	}
}
