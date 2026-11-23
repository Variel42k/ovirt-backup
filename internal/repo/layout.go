package repo

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Расположение объекта внутри хранилища:
//
//	jhvirt/<server>/<vm>/<YYYY>/<MM>/<DD>/<run-id>/
//	    run.json                     — метаданные запуска и список дисков
//	    vm-config.json               — конфигурация ВМ на момент бэкапа
//	    disk-00-<disk-id>.manifest   — карта экстентов и хэши чанков
//	    disk-00-<disk-id>.data       — сами данные
//
// Уровни даты - это то, что делает хранилище доступным для просмотра человеком с `ls` или
// S3 консолью, и они позволяют перечислению удержания в день без перечисления всего ведра.

// Root — это пространство имен верхнего уровня внутри цели, поэтому ведро или каталог могут
// использоваться совместно с другими инструментами.
const Root = "jhvirt"

// RunPrefix строит ключевой префикс одного резервного запуска. Она всегда заканчивается
// цифрой "/".
func RunPrefix(serverName, vmID, vmName string, at time.Time, runID string) string {
	at = at.UTC()
	return fmt.Sprintf("%s/%s/%s/%04d/%02d/%02d/%s/",
		Root,
		Segment(serverName),
		vmSegment(vmID, vmName),
		at.Year(), int(at.Month()), at.Day(),
		Segment(runID),
	)
}

// VMPrefix задаёт общий префикс всех копий одной ВМ для правил хранения
// и просмотра списка её бэкапов.
func VMPrefix(serverName, vmID, vmName string) string {
	return fmt.Sprintf("%s/%s/%s/", Root, Segment(serverName), vmSegment(vmID, vmName))
}

// ServerPrefix покрывает каждую резервную копию одного движка.
func ServerPrefix(serverName string) string {
	return fmt.Sprintf("%s/%s/", Root, Segment(serverName))
}

// RunManifestKey является объектом метаданных уровня выполнения.
func RunManifestKey(runPrefix string) string { return runPrefix + "run.json" }

// VMConfigKey — конфигурация хранимой ВМ.
func VMConfigKey(runPrefix string) string { return runPrefix + "vm-config.json" }

// LibvirtConfigKey является исходным доменом XML, хранящимся с резервной копией KVM.
// Проверяющий использует портативный профиль в run.json; Оригиналъ XML остается ревизскимъ
// и стихійно-восстановительнымъ.
func LibvirtConfigKey(runPrefix string) string { return runPrefix + "vm-config.xml" }

// DiskManifestKey — это карта протяженности и дайджесты чанков одного диска.
func DiskManifestKey(runPrefix string, index int, diskID string) string {
	return fmt.Sprintf("%sdisk-%02d-%s.manifest", runPrefix, index, Segment(diskID))
}

// DiskDataKey — чанк сгустка одного диска.
func DiskDataKey(runPrefix string, index int, diskID string) string {
	return fmt.Sprintf("%sdisk-%02d-%s.data", runPrefix, index, Segment(diskID))
}

func ArtifactManifestKey(runPrefix string, index int, diskID, kind string) string {
	return fmt.Sprintf("%sartifact-%02d-%s.%s.manifest", runPrefix, index, Segment(diskID), Segment(kind))
}

func ArtifactDataKey(runPrefix string, index int, diskID, kind string) string {
	return fmt.Sprintf("%sartifact-%02d-%s.%s.data", runPrefix, index, Segment(diskID), Segment(kind))
}

// OVAKey является экспортируемым устройством резервного копирования типа OVA.
func OVAKey(runPrefix, vmName string) string {
	return fmt.Sprintf("%s%s.ova", runPrefix, Segment(vmName))
}

// vmSegment предпочитает "<name>-<short-id>", чтобы человек мог распознать каталог, в то
// время как идентификатор сохраняет его уникальным по всем переименованиям.
func vmSegment(vmID, vmName string) string {
	short := vmID
	if len(short) > 8 {
		short = short[:8]
	}
	if vmName == "" {
		return Segment(vmID)
	}
	return Segment(vmName) + "-" + Segment(short)
}

// Segment делает произвольную строку безопасной для использования в качестве одного
// компонента пути/ключа на каждом сервере: нет разделителей, нет пробелов, нет символов,
// которые Windows, S3 или оболочка будет обрабатывать специально.
func Segment(s string) string {
	if s == "" {
		return "unnamed"
	}
	var b strings.Builder
	b.Grow(len(s))
	lastDash := false
	for _, r := range s {
		switch {
		case r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(unicode.ToLower(r))
			lastDash = false
		case r == '-' || r == '_' || r == '.':
			if !lastDash {
				b.WriteRune(r)
			}
			lastDash = false
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			// Не-ASCII буквы (кириллические имена ВМ здесь являются нормой)
			// транслитерируются, а не отбрасываются, поэтому каталоги остаются читаемыми.
			if tr, ok := translit[unicode.ToLower(r)]; ok {
				b.WriteString(tr)
				lastDash = false
				continue
			}
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "unnamed"
	}
	if len(out) > 96 {
		out = strings.Trim(out[:96], "-.")
	}
	return out
}

// translit maps Cyrillic to ASCII so that VM names like "БД-Продакшн" produce
// каталог, который оператор может читать и вводить.
var translit = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e",
	'ж': "zh", 'з': "z", 'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m",
	'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u",
	'ф': "f", 'х': "h", 'ц': "c", 'ч': "ch", 'ш': "sh", 'щ': "sch",
	'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",
}
