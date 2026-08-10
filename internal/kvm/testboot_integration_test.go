package kvm

import (
	"context"
	"os"
	"path"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"adveng/jh_virt/internal/backup"
	"adveng/jh_virt/internal/libvirtx"
)

// TestRealKVMGuestAgentBoot is an opt-in acceptance test against a real
// libvirt/KVM host. The fixture must be a bootable image with qemu-guest-agent
// enabled. It is cloned remotely first; the source image is never attached or
// modified by the test.
//
// Required environment:
//
//	JHV_TEST_KVM_HOST, JHV_TEST_KVM_USER, JHV_TEST_KVM_IMAGE
//	JHV_TEST_KVM_PASSWORD or JHV_TEST_KVM_KEY_FILE
//
// Optional: JHV_TEST_KVM_PORT, _SOCKET, _SCRATCH, _FORMAT, _ARCH, _MACHINE,
// _FIRMWARE, _SECURE_BOOT, _MEMORY_MIB, _VCPUS and _TIMEOUT_SEC.
func TestRealKVMGuestAgentBoot(t *testing.T) {
	host := os.Getenv("JHV_TEST_KVM_HOST")
	user := os.Getenv("JHV_TEST_KVM_USER")
	sourceImage := os.Getenv("JHV_TEST_KVM_IMAGE")
	if host == "" || user == "" || sourceImage == "" {
		t.Skip("реальный KVM-тест: задайте JHV_TEST_KVM_HOST, JHV_TEST_KVM_USER и JHV_TEST_KVM_IMAGE")
	}

	privateKey := ""
	if keyFile := os.Getenv("JHV_TEST_KVM_KEY_FILE"); keyFile != "" {
		raw, err := os.ReadFile(keyFile)
		if err != nil {
			t.Fatalf("чтение SSH-ключа: %v", err)
		}
		privateKey = string(raw)
	}
	port := envInt("JHV_TEST_KVM_PORT", 22)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(envInt("JHV_TEST_KVM_TIMEOUT_SEC", 300)+120)*time.Second)
	defer cancel()

	conn, err := libvirtx.Connect(ctx, libvirtx.Config{
		Host: host, Port: port, User: user, Password: os.Getenv("JHV_TEST_KVM_PASSWORD"),
		PrivateKey: privateKey, HostKey: os.Getenv("JHV_TEST_KVM_HOST_KEY"),
		Socket: os.Getenv("JHV_TEST_KVM_SOCKET"), ConnectTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("подключение к реальному libvirt: %v", err)
	}
	defer conn.Close()

	scratch := os.Getenv("JHV_TEST_KVM_SCRATCH")
	if scratch == "" {
		scratch = "/var/lib/libvirt/qemu"
	}
	id := uuid.NewString()
	rootImage := path.Join(scratch, "jhv-real-test-"+id+"-root.img")
	dataImage := path.Join(scratch, "jhv-real-test-"+id+"-data.raw")
	cleanup := func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		_, _ = conn.Run(cleanupCtx, "rm -f "+shellQuote(rootImage)+" "+shellQuote(dataImage))
	}
	t.Cleanup(cleanup)

	if _, err := conn.Run(ctx, "cp --reflink=auto --sparse=always -- "+shellQuote(sourceImage)+" "+shellQuote(rootImage)); err != nil {
		t.Fatalf("клонирование тестового образа: %v", err)
	}
	if _, err := conn.Run(ctx, "truncate -s 64M "+shellQuote(dataImage)); err != nil {
		t.Fatalf("создание второго тестового диска: %v", err)
	}

	format := os.Getenv("JHV_TEST_KVM_FORMAT")
	if format == "" {
		format = "qcow2"
	}
	arch := os.Getenv("JHV_TEST_KVM_ARCH")
	if arch == "" {
		arch = "x86_64"
	}
	machine := os.Getenv("JHV_TEST_KVM_MACHINE")
	if machine == "" {
		machine = backup.PortableMachine(arch, "")
	}
	firmware := os.Getenv("JHV_TEST_KVM_FIRMWARE")
	if firmware == "" {
		firmware = "bios"
	}

	driver := NewDriver(conn, Config{ScratchDir: scratch}, nil, zerolog.Nop())
	result, err := driver.RunBootTest(ctx, BootTest{
		Name: "real-kvm-acceptance",
		Disks: []BootDisk{
			{RemoteImage: rootImage, Format: format, Target: "vda", Bus: "virtio", BootOrder: 1},
			{RemoteImage: dataImage, Format: "raw", Target: "vdb", Bus: "virtio"},
		},
		Profile: &backup.VMProfile{
			Architecture: arch, Machine: machine, Firmware: firmware,
			SecureBoot: envBool("JHV_TEST_KVM_SECURE_BOOT"),
			MemoryMiB:  envInt("JHV_TEST_KVM_MEMORY_MIB", 2048),
			VCPUs:      envInt("JHV_TEST_KVM_VCPUS", 2),
		},
		Timeout: time.Duration(envInt("JHV_TEST_KVM_TIMEOUT_SEC", 300)) * time.Second,
	}, zerolog.Nop())
	if err != nil {
		t.Fatalf("реальный запуск KVM: %v", err)
	}
	if !result.Passed() {
		t.Fatalf("гость не подтвердил загрузку: %s; примечания: %v", result.Summary(), result.Notes)
	}
	t.Logf("реальная VM загрузилась: %s, ОС: %s", result.Hostname, result.GuestOS)
}

func envBool(name string) bool {
	value, err := strconv.ParseBool(os.Getenv(name))
	return err == nil && value
}

func envInt(name string, fallback int) int {
	if value, err := strconv.Atoi(os.Getenv(name)); err == nil && value > 0 {
		return value
	}
	return fallback
}
