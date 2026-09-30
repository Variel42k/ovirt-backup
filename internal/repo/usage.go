package repo

// DiskUsage — свободное и полное место файловой системы, где лежит path.
// Свободное — то, что доступно непривилегированному процессу.
func DiskUsage(path string) (free, total int64, err error) {
	return diskUsage(path)
}
