package api

import (
	"testing"
)

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
