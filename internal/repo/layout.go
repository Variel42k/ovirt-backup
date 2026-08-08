package repo

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Object layout inside a repository:
//
//	jhvirt/<server>/<vm>/<YYYY>/<MM>/<DD>/<run-id>/
//	    run.json                     — метаданные запуска и список дисков
//	    vm-config.json               — конфигурация ВМ на момент бэкапа
//	    disk-00-<disk-id>.manifest   — карта экстентов и хэши чанков
//	    disk-00-<disk-id>.data       — сами данные
//
// The date levels are what makes a repository browsable by a human with `ls`
// or an S3 console, and they let retention enumerate a day without listing the
// whole bucket.

// Root is the top-level namespace inside a target, so a bucket or directory
// can be shared with other tools.
const Root = "jhvirt"

// RunPrefix builds the key prefix of one backup run. It always ends with "/".
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

// VMPrefix is the prefix covering every backup of one VM, used by retention
// and by the "show me everything for this VM" view.
func VMPrefix(serverName, vmID, vmName string) string {
	return fmt.Sprintf("%s/%s/%s/", Root, Segment(serverName), vmSegment(vmID, vmName))
}

// ServerPrefix covers every backup of one engine.
func ServerPrefix(serverName string) string {
	return fmt.Sprintf("%s/%s/", Root, Segment(serverName))
}

// RunManifestKey is the run-level metadata object.
func RunManifestKey(runPrefix string) string { return runPrefix + "run.json" }

// VMConfigKey is the stored VM configuration.
func VMConfigKey(runPrefix string) string { return runPrefix + "vm-config.json" }

// DiskManifestKey is the extent map and chunk digests of one disk.
func DiskManifestKey(runPrefix string, index int, diskID string) string {
	return fmt.Sprintf("%sdisk-%02d-%s.manifest", runPrefix, index, Segment(diskID))
}

// DiskDataKey is the chunk blob of one disk.
func DiskDataKey(runPrefix string, index int, diskID string) string {
	return fmt.Sprintf("%sdisk-%02d-%s.data", runPrefix, index, Segment(diskID))
}

// OVAKey is the exported appliance of an OVA-type backup.
func OVAKey(runPrefix, vmName string) string {
	return fmt.Sprintf("%s%s.ova", runPrefix, Segment(vmName))
}

// vmSegment prefers "<name>-<short-id>" so a human can recognise the directory,
// while the id keeps it unique across renames.
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

// Segment makes an arbitrary string safe for use as one path/key component on
// every backend: no separators, no spaces, no characters that Windows, S3 or a
// shell would treat specially.
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
			// Non-ASCII letters (Cyrillic VM names are the norm here) are
			// transliterated rather than dropped, so directories stay readable.
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
// a directory an operator can still read and type.
var translit = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e",
	'ж': "zh", 'з': "z", 'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m",
	'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u",
	'ф': "f", 'х': "h", 'ц': "c", 'ч': "ch", 'ш': "sh", 'щ': "sch",
	'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",
}
