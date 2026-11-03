package model

// ValidBackupCompression includes the empty value used to inherit the system
// setting. The effective algorithm is captured on each run and in its manifests.
func ValidBackupCompression(name string) bool {
	switch name {
	case "", "none", "zstd", "gzip", "s2", "lz4", "brotli":
		return true
	default:
		return false
	}
}
