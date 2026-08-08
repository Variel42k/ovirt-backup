package backup

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// qemu-img is optional. It is only needed for two conveniences: exporting a
// restored image as a qcow2 that any hypervisor can open, and running
// `qemu-img check` as an extra, independent opinion on a restored image.
// Everything else in this service works without it.

// FindQemuImg resolves the qemu-img binary. An explicit path from the config
// wins; otherwise PATH is searched.
func FindQemuImg(configured string) (string, error) {
	if configured != "" {
		if _, err := exec.LookPath(configured); err != nil {
			return "", fmt.Errorf("qemu-img по указанному пути %q недоступен: %w", configured, err)
		}
		return configured, nil
	}
	path, err := exec.LookPath("qemu-img")
	if err != nil {
		return "", fmt.Errorf("qemu-img не найден в PATH: %w", err)
	}
	return path, nil
}

// QemuImgAvailable reports whether the tool can be used, for feature flags in
// the UI.
func QemuImgAvailable(configured string) bool {
	_, err := FindQemuImg(configured)
	return err == nil
}

// ConvertToQcow2 converts a raw image into a compressed qcow2.
func ConvertToQcow2(ctx context.Context, configured, src, dst string) error {
	bin, err := FindQemuImg(configured)
	if err != nil {
		return err
	}
	// -S 4k keeps the output sparse; without it a mostly-empty raw image
	// becomes a fully allocated qcow2.
	cmd := exec.CommandContext(ctx, bin, "convert", "-p", "-O", "qcow2", "-S", "4k", src, dst)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img convert: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// QemuImgCheck runs a consistency check over an image and returns its output.
func QemuImgCheck(ctx context.Context, configured, path string) (string, error) {
	bin, err := FindQemuImg(configured)
	if err != nil {
		return "", err
	}
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(checkCtx, bin, "check", "-f", "qcow2", path)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		// qemu-img check exits non-zero for "leaked clusters" too, which is a
		// warning rather than corruption; the caller sees the text and decides.
		return text, fmt.Errorf("qemu-img check: %w", err)
	}
	return text, nil
}

// QemuImgInfo returns the tool's description of an image, used to confirm a
// restored file really is what it claims to be.
func QemuImgInfo(ctx context.Context, configured, path string) (string, error) {
	bin, err := FindQemuImg(configured)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, bin, "info", "--output=json", path)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("qemu-img info: %w", err)
	}
	return string(out), nil
}
