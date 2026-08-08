//go:build !windows

package repo

import "golang.org/x/sys/unix"

// diskUsage reports free and total bytes of the filesystem holding path.
func diskUsage(path string) (free, total int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	// Bavail is what an unprivileged writer can actually use; Bfree includes
	// the reserved blocks and would overstate the headroom.
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize), nil
}
