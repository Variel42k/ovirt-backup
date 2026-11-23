package webui

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Handler раздаёт веб-сборку и возвращает index.html для клиентских маршрутов.
func Handler(dir string) http.Handler {
	fileServer := http.FileServer(http.Dir(dir))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "веб-интерфейс не собран: соберите web/ и положите результат в " + dir,
				"code":  "spa_missing",
			})
			return
		}

		// Оболочку приложения браузер обязан перепроверять при каждой загрузке.
		// В ней записаны имена файлов сборки, а те содержат хэш содержимого:
		// после обновления сохранённая в кэше оболочка просит куски, которых в
		// новой сборке уже нет. Ломается при этом не всё сразу — открытые
		// страницы лежат в памяти и работают, — а только те разделы, куда
		// пользователь ещё не заходил: их код подгружается лениво и молча
		// падает на 404. Выглядит это как «перестала работать кнопка», и связь
		// с обновлением не очевидна совершенно.
		//
		// no-cache, а не no-store: перепроверка почти бесплатна, ответ 304
		// оставляет файл в кэше и не гоняет его заново.
		serveShell := func() {
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeFile(w, r, filepath.Join(dir, "index.html"))
		}

		clean := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if clean == "." || clean == "/" {
			serveShell()
			return
		}
		target := filepath.Join(dir, clean)
		if info, err := os.Stat(target); err == nil && !info.IsDir() {
			// Обратная сторона того же правила: имя файла сборки меняется
			// вместе с содержимым, поэтому его можно держать в кэше сколь
			// угодно долго. Дешёвая перепроверка оболочки и вечный кэш для
			// всего остального — одно решение, разнесённое на две ветки.
			if strings.HasPrefix(r.URL.Path, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			fileServer.ServeHTTP(w, r)
			return
		}

		// Недостающий статический файл должен быть 404, а не оболочка приложения.
		//
		// Для маршрутов SPA вроде /backups при перезагрузке возвращаем index.html. Если
		// отсутствует скрипт из старой сборки, ответ HTML вместо JavaScript вызвал бы ошибку
		// "Unexpected token '<'". Поэтому для отсутствующего скрипта возвращаем 404.
		if isStaticAsset(clean) {
			http.NotFound(w, r)
			return
		}
		serveShell()
	})
}

// isStaticAsset сообщает, должен ли путь быть файлом, а не маршрутом на стороне клиента.
//
// Клиентские маршруты этого приложения не имеют расширения (/backups,
// /servers/<идентификатор>), в то время как каждый встроенный актив имеет. Это эвристика,
// но неудача, которую она предотвращает, гораздо более запутанна, чем та, которую она может
// вызвать.
func isStaticAsset(clean string) bool {
	if strings.HasPrefix(clean, "assets"+string(filepath.Separator)) || strings.HasPrefix(clean, "assets/") {
		return true
	}
	return filepath.Ext(clean) != ""
}
