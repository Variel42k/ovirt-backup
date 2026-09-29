package backup

import (
	"fmt"
	"os"
	"strings"
)

// ensureTempBase returns the effective scratch directory and makes sure it
// exists. The explicit error names backup.temp_dir because a bare mkdir error
// after a snapshot has been created is too late and gives the operator no clue
// which host mount needs fixing.
func ensureTempBase(configured string) (string, error) {
	base := strings.TrimSpace(configured)
	if base == "" {
		base = os.TempDir()
	}
	if err := os.MkdirAll(base, 0o750); err != nil {
		return "", fmt.Errorf("backup.temp_dir %q нельзя создать: %w", base, err)
	}
	return base, nil
}

// checkTempWorkspace verifies actual write access by creating a directory.
// Mode bits alone are insufficient for bind mounts, NFS, SELinux and a
// container running under a numeric UID.
func checkTempWorkspace(configured string) error {
	base, err := ensureTempBase(configured)
	if err != nil {
		return err
	}
	probe, err := os.MkdirTemp(base, ".jhv-temp-probe-")
	if err != nil {
		return fmt.Errorf("backup.temp_dir %q недоступен для записи пользователю службы: %w", base, err)
	}
	if err := os.RemoveAll(probe); err != nil {
		return fmt.Errorf("backup.temp_dir %q: пробный каталог создан, но не удаляется: %w", base, err)
	}
	return nil
}

func makeTempWorkspace(configured, pattern string) (string, error) {
	base, err := ensureTempBase(configured)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(base, pattern)
	if err != nil {
		return "", fmt.Errorf("backup.temp_dir %q недоступен для записи пользователю службы: %w", base, err)
	}
	return dir, nil
}
