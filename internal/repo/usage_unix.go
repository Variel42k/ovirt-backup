//go:build !windows

package repo

import "golang.org/x/sys/unix"

// diskUsage сообщает о свободных и полных байтах пути удержания файловой системы.
func diskUsage(path string) (free, total int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	// Bavail — это то, что может использовать непривилегированный писатель; Bfree включает
	// в себя зарезервированные блоки и будет преувеличивать пространство.
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize), nil
}
