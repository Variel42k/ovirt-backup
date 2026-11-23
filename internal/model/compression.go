package model

// ValidBackupCompression допускает пустое значение для наследования системной настройки.
// Выбранный алгоритм сохраняется в запуске и его манифестах.
func ValidBackupCompression(name string) bool {
	switch name {
	case "", "none", "zstd", "gzip", "s2", "lz4", "brotli":
		return true
	default:
		return false
	}
}
