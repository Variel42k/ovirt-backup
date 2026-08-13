package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"adveng/jh_virt/internal/config"
)

// Оболочку приложения браузер обязан перепроверять, файлы сборки — нет.
//
// Без этого после обновления интерфейс ломается частично и необъяснимо: старый
// index.html из кэша просит куски по прежним именам, а имена содержат хэш
// содержимого и в новой сборке уже другие. Открытые страницы продолжают
// работать, а разделы, куда пользователь ещё не заходил, перестают
// открываться — со стороны это «перестала работать кнопка».
func TestAppShellIsRevalidatedAndAssetsAreCached(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html></html>"), 0o600); err != nil {
		t.Fatalf("index.html: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o750); err != nil {
		t.Fatalf("каталог assets: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "index-abc123.js"), []byte("//"), 0o600); err != nil {
		t.Fatalf("файл сборки: %v", err)
	}

	var cfg config.Config
	cfg.Server.SPADir = dir
	handler := (&Server{cfg: cfg}).spaHandler()

	cases := []struct {
		path string
		want string
		why  string
	}{
		{"/", "no-cache", "оболочку нужно перепроверять на каждой загрузке"},
		{"/settings", "no-cache", "клиентский маршрут отдаёт ту же оболочку"},
		{"/assets/index-abc123.js", "public, max-age=31536000, immutable", "имя файла меняется вместе с содержимым"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: код %d, ожидался 200", c.path, rec.Code)
			continue
		}
		if got := rec.Header().Get("Cache-Control"); got != c.want {
			t.Errorf("%s: Cache-Control %q, ожидался %q — %s", c.path, got, c.want, c.why)
		}
	}
}

// Клиентский маршрут перезагружается в приложение, отсутствующий файл — нет.
//
// Отдать index.html вместо пропавшего скрипта значит вернуть браузеру HTML с
// типом JavaScript: страница умирает с «Unexpected token '<'», из чего причину
// не видно. Так и происходит после обновления, когда закэшированный index.html
// просит чанк, которого в новой сборке уже нет.
func TestStaticAssetsAreNotServedTheAppShell(t *testing.T) {
	assets := []string{
		"assets/index-C6x7LhIL.js",
		"assets/index-abc.css",
		"favicon.ico",
		"manifest.webmanifest",
		"logo.svg",
	}
	for _, path := range assets {
		if !isStaticAsset(path) {
			t.Errorf("%q — файл, для него нужен 404, а не оболочка приложения", path)
		}
	}

	routes := []string{
		"backups",
		"servers",
		"servers/9d1c2f4a-0000-0000-0000-000000000001",
		"servers/abc/vms/def",
		"settings",
		"retention",
	}
	for _, path := range routes {
		if isStaticAsset(path) {
			t.Errorf("%q — клиентский маршрут, он должен открывать приложение", path)
		}
	}
}
