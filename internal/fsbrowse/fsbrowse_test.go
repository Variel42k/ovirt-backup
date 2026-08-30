package fsbrowse

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func testRoots(t *testing.T) ([]Root, string) {
	t.Helper()
	base := t.TempDir()
	// Настоящий путь корня может отличаться от выданного t.TempDir(): на macOS
	// /var — ссылка на /private/var. Сравнение идёт по разрешённому пути.
	real, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"nightly", "nightly/2026-08", "archive"} {
		if err := os.MkdirAll(filepath.Join(real, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return []Root{NewRoot("backups", "Копии", real)}, real
}

// Первое, что делает выбиралка, — показывает корни. Файловой системы при этом
// не видно вовсе.
func TestListWithoutRootShowsOnlyRoots(t *testing.T) {
	roots, _ := testRoots(t)

	listing, err := List(roots, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Roots) != 1 || listing.Roots[0].ID != "backups" {
		t.Errorf("корни не отданы: %+v", listing.Roots)
	}
	if len(listing.Entries) != 0 {
		t.Errorf("без корня не должно быть содержимого, получено %d", len(listing.Entries))
	}
	if listing.Parent != nil {
		t.Errorf("в списке корней не должно быть перехода вверх: %v", *listing.Parent)
	}
}

func TestListShowsDirectoriesInsideRoot(t *testing.T) {
	roots, _ := testRoots(t)

	listing, err := List(roots, "backups", "")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, entry := range listing.Entries {
		names[entry.Name] = true
	}
	if !names["nightly"] || !names["archive"] {
		t.Errorf("каталоги не перечислены: %+v", listing.Entries)
	}
	if listing.Parent != nil {
		t.Errorf("из самого корня подниматься некуда, а предложено %q", *listing.Parent)
	}
	if !listing.Writable {
		t.Error("временный каталог должен быть доступен на запись")
	}
}

// Именованный корень не должен выдавать своё расположение: конфигурация прячет
// его намеренно, и выбиралка не может стать способом это обойти.
func TestNamedRootHidesItsLocation(t *testing.T) {
	_, base := testRoots(t)
	roots := []Root{NewNamedRoot("docs", "Документы", base)}

	listing, err := List(roots, "docs", "nightly")
	if err != nil {
		t.Fatal(err)
	}
	if listing.Roots[0].Path != "" {
		t.Errorf("расположение именованного корня отдано наружу: %q", listing.Roots[0].Path)
	}
	if listing.Absolute != "" {
		t.Errorf("полный путь отдан наружу: %q", listing.Absolute)
	}
	if listing.Path != "nightly" {
		t.Errorf("путь внутри корня = %q, ожидался относительный", listing.Path)
	}
	for _, entry := range listing.Entries {
		if filepath.IsAbs(entry.Path) {
			t.Errorf("в списке абсолютный путь: %q", entry.Path)
		}
	}
}

// Подъём выше корня — самый простой способ выйти за пределы разрешённого, и
// именно его пробуют первым.
func TestListRefusesEscapeAboveRoot(t *testing.T) {
	roots, base := testRoots(t)

	for _, attempt := range []string{
		"..",
		"../..",
		"nightly/../../..",
		filepath.Dir(base),
	} {
		if _, err := List(roots, "backups", attempt); !errors.Is(err, ErrOutsideRoots) {
			t.Errorf("путь %q не отвергнут: %v", attempt, err)
		}
	}
}

func TestListRefusesUnknownRoot(t *testing.T) {
	roots, _ := testRoots(t)

	if _, err := List(roots, "чужой", ""); !errors.Is(err, ErrOutsideRoots) {
		t.Errorf("неизвестный корень не отвергнут: %v", err)
	}
}

// Абсолютный путь внутри корня принимается: форма редактирования хранит именно
// его, и повторное открытие выбиралки должно начинаться там же.
func TestAbsolutePathInsideRootIsAccepted(t *testing.T) {
	roots, base := testRoots(t)

	listing, err := List(roots, "backups", filepath.Join(base, "nightly"))
	if err != nil {
		t.Fatalf("абсолютный путь внутри корня отвергнут: %v", err)
	}
	if listing.Path != "nightly" {
		t.Errorf("путь = %q, ожидался nightly", listing.Path)
	}
}

// Ссылка внутри корня, ведущая наружу, не должна ни открываться, ни
// показываться в списке: иначе выбор каталога становится способом обойти сами
// корни.
func TestSymlinkOutOfRootIsNeitherListedNorOpened(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("создание ссылок под Windows требует отдельных прав")
	}
	roots, base := testRoots(t)

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(base, "наружу")); err != nil {
		t.Skipf("ссылки недоступны: %v", err)
	}

	listing, err := List(roots, "backups", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range listing.Entries {
		if entry.Name == "наружу" {
			t.Error("ссылка за пределы корня показана в списке")
		}
	}
	if _, err := List(roots, "backups", "наружу"); !errors.Is(err, ErrOutsideRoots) {
		t.Errorf("переход по ссылке за пределы корня не отвергнут: %v", err)
	}
}

// Несуществующий путь и путь наружу отвечают одинаково: разница между ними
// сама по себе рассказывала бы о том, что на диске есть.
func TestMissingPathIsIndistinguishableFromForbidden(t *testing.T) {
	roots, _ := testRoots(t)

	_, missing := List(roots, "backups", "нет-такого")
	_, forbidden := List(roots, "backups", "..")
	if !errors.Is(missing, ErrOutsideRoots) || !errors.Is(forbidden, ErrOutsideRoots) {
		t.Fatalf("ответы разошлись: %v и %v", missing, forbidden)
	}
	if missing.Error() != forbidden.Error() {
		t.Errorf("по тексту ошибки различимо, существует ли путь: %q против %q",
			missing, forbidden)
	}
}

func TestParentStaysInsideRoot(t *testing.T) {
	roots, _ := testRoots(t)

	listing, err := List(roots, "backups", "nightly/2026-08")
	if err != nil {
		t.Fatal(err)
	}
	if listing.Parent == nil || *listing.Parent != "nightly" {
		t.Errorf("переход вверх ведёт в %v", listing.Parent)
	}

	// На один уровень выше родитель — сам корень, то есть пустая строка, а не
	// отсутствие перехода.
	listing, err = List(roots, "backups", "nightly")
	if err != nil {
		t.Fatal(err)
	}
	if listing.Parent == nil || *listing.Parent != "" {
		t.Errorf("из первого уровня переход вверх = %v, ожидался корень", listing.Parent)
	}
}

func TestEmptyDirectoryIsMarked(t *testing.T) {
	roots, _ := testRoots(t)

	listing, err := List(roots, "backups", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range listing.Entries {
		switch entry.Name {
		case "archive":
			if !entry.Empty {
				t.Error("пустой каталог не помечен пустым")
			}
		case "nightly":
			if entry.Empty {
				t.Error("непустой каталог помечен пустым")
			}
		}
	}
}
