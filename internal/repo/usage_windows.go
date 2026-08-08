//go:build windows

package repo

import "golang.org/x/sys/windows"

// diskUsage reports free and total bytes of the volume holding path.
func diskUsage(path string) (free, total int64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var freeForCaller, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeForCaller, &totalBytes, &totalFree); err != nil {
		return 0, 0, err
	}
	return int64(freeForCaller), int64(totalBytes), nil
}
