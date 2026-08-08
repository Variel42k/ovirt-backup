package kvm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/rs/zerolog"
)

// The strongest verification available: boot the restored image and wait for
// the guest to answer.
//
// Every other check reasons about bytes. This one asks the operating system
// inside the backup whether it came up — which is the actual question behind
// "is this backup any good".
//
// Two safety properties are non-negotiable and are enforced here rather than
// left to configuration:
//
//   - The test domain has no network interfaces at all. A restored production
//     server booting onto the live network would take over addresses, register
//     with directories and start writing to shared storage. Isolation is not a
//     precaution here, it is the difference between a test and an incident.
//   - The domain is transient and boots a copy. Nothing it does can reach the
//     original disks, and it disappears when destroyed.

// BootTest describes a boot verification.
type BootTest struct {
	// RemoteImage — путь к восстановленному образу на гипервизоре.
	RemoteImage string
	// Format образа: raw или qcow2.
	Format string

	MemoryMiB int
	VCPUs     int
	// Timeout — сколько ждать отклика гостевого агента.
	Timeout time.Duration
	// KeepOnFailure оставляет ВМ и образ для разбора, если тест не прошёл.
	KeepOnFailure bool
	// Name используется в имени временного домена.
	Name string
}

func (t BootTest) withDefaults() BootTest {
	if t.Format == "" {
		t.Format = "raw"
	}
	if t.MemoryMiB <= 0 {
		t.MemoryMiB = 2048
	}
	if t.VCPUs <= 0 {
		t.VCPUs = 2
	}
	if t.Timeout <= 0 {
		t.Timeout = 5 * time.Minute
	}
	if t.Name == "" {
		t.Name = "restore"
	}
	return t
}

// BootTestResult reports what happened.
type BootTestResult struct {
	DomainName   string        `json:"domain_name"`
	Started      bool          `json:"started"`
	AgentReplied bool          `json:"agent_replied"`
	Elapsed      time.Duration `json:"elapsed"`
	GuestOS      string        `json:"guest_os,omitempty"`
	Hostname     string        `json:"hostname,omitempty"`
	Notes        []string      `json:"notes,omitempty"`
}

// Passed reports whether the guest actually came up.
func (r *BootTestResult) Passed() bool { return r.Started && r.AgentReplied }

// Summary renders the outcome for a report.
func (r *BootTestResult) Summary() string {
	switch {
	case r.Passed():
		text := fmt.Sprintf("гостевая система поднялась за %s и ответила через агента",
			r.Elapsed.Round(time.Second))
		if r.Hostname != "" {
			text += ", имя хоста: " + r.Hostname
		}
		return text
	case r.Started:
		return fmt.Sprintf("ВМ запустилась, но гостевой агент не ответил за %s — "+
			"либо агент не установлен в этой системе, либо она не загрузилась",
			r.Elapsed.Round(time.Second))
	default:
		return "ВМ не удалось запустить"
	}
}

// UploadOptions tunes how an image is streamed onto the hypervisor.
type UploadOptions struct {
	// Compressed означает, что src уже сжат gzip и на той стороне поток надо
	// пропустить через распаковщик.
	Compressed bool
}

// RemoteHasGzip reports whether the hypervisor can decompress a gzip stream.
//
// It matters because a restored image is streamed at full logical size: the
// holes in a sparse disk are real zeros on the wire. Compressing them away
// turns a mostly-empty terabyte into a few megabytes of traffic, and gzip is
// the one decompressor present on effectively every Linux host. Where it is
// missing the transfer still works, just at full size.
func (d *Driver) RemoteHasGzip(ctx context.Context) bool {
	out, err := d.conn.Run(ctx, "command -v gzip >/dev/null 2>&1 && echo yes || echo no")
	return err == nil && strings.Contains(string(out), "yes")
}

// UploadImage streams a reconstructed image onto the hypervisor.
func (d *Driver) UploadImage(ctx context.Context, src io.Reader, remotePath string, opt UploadOptions) error {
	dir := path.Dir(remotePath)
	if _, err := d.conn.Run(ctx, fmt.Sprintf("mkdir -p '%s'", dir)); err != nil {
		return err
	}

	// dd with a large block size rather than `cat`: it reports write errors on
	// a full filesystem instead of silently truncating. conv=sparse keeps the
	// zero regions of the image from occupying real blocks — a 500 GiB disk
	// with 20 GiB of data lands as a 20 GiB file.
	cmd := fmt.Sprintf("dd of='%s' bs=4M conv=sparse,fsync status=none", remotePath)
	if opt.Compressed {
		cmd = "gzip -dc | " + cmd
	}

	if err := d.conn.RunWithStdin(ctx, cmd, src); err != nil {
		d.RemoveRemote(context.WithoutCancel(ctx), remotePath)
		return fmt.Errorf("передача образа на %s: %w", d.conn.Host(), err)
	}
	return nil
}

// AvailableBytes reports the free space of the filesystem holding dir.
//
// It is checked before an upload because the alternative is discovering the
// shortfall halfway through: a hypervisor with a full root filesystem stops
// being a hypervisor, and a verification must never be the thing that takes
// production down.
func (d *Driver) AvailableBytes(ctx context.Context, dir string) (int64, error) {
	// -P forces the POSIX single-line format, -k fixes the unit at 1 KiB, so
	// the numbers mean the same thing on any Unix the host might be.
	out, err := d.conn.Run(ctx, fmt.Sprintf("df -Pk '%s'", dir))
	if err != nil {
		return 0, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("неожиданный вывод df для %s", dir)
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return 0, fmt.Errorf("неожиданный вывод df для %s", dir)
	}
	kib, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("разбор свободного места на %s: %w", dir, err)
	}
	return kib * 1024, nil
}

// RemoveRemote deletes a file left on the hypervisor.
func (d *Driver) RemoveRemote(ctx context.Context, remotePath string) {
	if remotePath == "" {
		return
	}
	if _, err := d.conn.Run(ctx, fmt.Sprintf("rm -f '%s'", remotePath)); err != nil {
		d.log.Warn().Err(err).Str("файл", remotePath).Msg("не удалён временный файл на гипервизоре")
	}
}

// RunBootTest boots an image in an isolated transient domain and waits for the
// guest agent.
func (d *Driver) RunBootTest(ctx context.Context, test BootTest, log zerolog.Logger) (*BootTestResult, error) {
	test = test.withDefaults()
	if test.RemoteImage == "" {
		return nil, fmt.Errorf("не указан путь к образу на гипервизоре")
	}

	domainName := fmt.Sprintf("jhv-verify-%s-%d", sanitiseName(test.Name), time.Now().Unix())
	result := &BootTestResult{DomainName: domainName}

	xml := bootTestDomainXML(domainName, test)
	log.Info().Str("домен", domainName).Str("образ", test.RemoteImage).
		Msg("пробный запуск ВМ из бэкапа (без сети)")

	started := time.Now()
	dom, err := d.conn.Libvirt().DomainCreateXML(xml, 0)
	if err != nil {
		return result, fmt.Errorf("запуск проверочной ВМ: %w", err)
	}
	result.Started = true

	cleanup := func() {
		// Detached context: the test domain must go away even if the
		// verification was cancelled, or it keeps consuming host memory.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()

		if err := d.conn.Libvirt().DomainDestroy(dom); err != nil &&
			!strings.Contains(strings.ToLower(err.Error()), "not running") {
			log.Error().Err(err).Str("домен", domainName).
				Msg("НЕ УДАЛОСЬ ОСТАНОВИТЬ проверочную ВМ — остановите её вручную")
		}
		if _, err := d.conn.Run(cleanupCtx, fmt.Sprintf("rm -f '%s'", test.RemoteImage)); err != nil {
			log.Warn().Err(err).Msg("не удалось удалить проверочный образ")
		}
	}

	agentInfo, err := d.waitForGuestAgent(ctx, dom, test.Timeout)
	result.Elapsed = time.Since(started)

	if err != nil {
		result.Notes = append(result.Notes, err.Error())
		if test.KeepOnFailure {
			result.Notes = append(result.Notes,
				fmt.Sprintf("ВМ %s и образ %s оставлены для разбора — удалите их вручную",
					domainName, test.RemoteImage))
			log.Warn().Str("домен", domainName).Msg("проверочная ВМ оставлена по настройке")
			return result, nil
		}
		cleanup()
		return result, nil
	}

	result.AgentReplied = true
	result.Hostname = agentInfo.hostname
	result.GuestOS = agentInfo.os
	cleanup()

	log.Info().Str("домен", domainName).Dur("за", result.Elapsed).
		Str("хост", result.Hostname).Msg("гостевая система из бэкапа поднялась")
	return result, nil
}

type guestAgentInfo struct {
	hostname string
	os       string
}

// waitForGuestAgent polls the guest agent until it answers.
//
// A guest that boots but has no agent installed cannot be distinguished from
// one that never booted, which is why the result says so explicitly rather
// than reporting a bare failure.
func (d *Driver) waitForGuestAgent(ctx context.Context, dom golibvirt.Domain, timeout time.Duration) (guestAgentInfo, error) {
	deadline := time.Now().Add(timeout)
	var info guestAgentInfo

	for {
		if err := ctx.Err(); err != nil {
			return info, err
		}

		// guest-ping is the cheapest call that proves the agent is alive.
		reply, err := d.conn.Libvirt().QEMUDomainAgentCommand(dom,
			`{"execute":"guest-ping"}`, 5, 0)
		if err == nil && len(reply) > 0 {
			info.hostname = d.agentHostname(dom)
			info.os = d.agentOSName(dom)
			return info, nil
		}

		if time.Now().After(deadline) {
			return info, fmt.Errorf("гостевой агент не ответил за %s", timeout)
		}
		select {
		case <-ctx.Done():
			return info, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (d *Driver) agentHostname(dom golibvirt.Domain) string {
	reply, err := d.conn.Libvirt().QEMUDomainAgentCommand(dom,
		`{"execute":"guest-get-host-name"}`, 5, 0)
	if err != nil || len(reply) == 0 {
		return ""
	}
	var parsed struct {
		Return struct {
			HostName string `json:"host-name"`
		} `json:"return"`
	}
	if json.Unmarshal([]byte(reply[0]), &parsed) != nil {
		return ""
	}
	return parsed.Return.HostName
}

func (d *Driver) agentOSName(dom golibvirt.Domain) string {
	reply, err := d.conn.Libvirt().QEMUDomainAgentCommand(dom,
		`{"execute":"guest-get-osinfo"}`, 5, 0)
	if err != nil || len(reply) == 0 {
		return ""
	}
	var parsed struct {
		Return struct {
			PrettyName string `json:"pretty-name"`
			Name       string `json:"name"`
			Version    string `json:"version"`
		} `json:"return"`
	}
	if json.Unmarshal([]byte(reply[0]), &parsed) != nil {
		return ""
	}
	if parsed.Return.PrettyName != "" {
		return parsed.Return.PrettyName
	}
	return strings.TrimSpace(parsed.Return.Name + " " + parsed.Return.Version)
}

// bootTestDomainXML builds the transient domain used for the test.
func bootTestDomainXML(name string, test BootTest) string {
	var b strings.Builder
	b.WriteString("<domain type='kvm'>\n")
	b.WriteString("  <name>" + name + "</name>\n")
	b.WriteString(fmt.Sprintf("  <memory unit='MiB'>%d</memory>\n", test.MemoryMiB))
	b.WriteString(fmt.Sprintf("  <vcpu>%d</vcpu>\n", test.VCPUs))
	b.WriteString("  <os>\n")
	b.WriteString("    <type arch='x86_64'>hvm</type>\n")
	b.WriteString("    <boot dev='hd'/>\n")
	b.WriteString("  </os>\n")
	b.WriteString("  <features><acpi/><apic/></features>\n")
	b.WriteString("  <cpu mode='host-passthrough'/>\n")
	b.WriteString("  <clock offset='utc'/>\n")
	// A guest that reboots or crashes must not loop: the test is over either way.
	b.WriteString("  <on_poweroff>destroy</on_poweroff>\n")
	b.WriteString("  <on_reboot>destroy</on_reboot>\n")
	b.WriteString("  <on_crash>destroy</on_crash>\n")
	b.WriteString("  <devices>\n")
	b.WriteString("    <disk type='file' device='disk'>\n")
	b.WriteString(fmt.Sprintf("      <driver name='qemu' type='%s' cache='unsafe'/>\n", test.Format))
	b.WriteString(fmt.Sprintf("      <source file='%s'/>\n", test.RemoteImage))
	b.WriteString("      <target dev='vda' bus='virtio'/>\n")
	b.WriteString("    </disk>\n")
	// Deliberately no <interface>: the restored guest must not reach the
	// network it thinks it owns.
	b.WriteString("    <channel type='unix'>\n")
	b.WriteString("      <target type='virtio' name='org.qemu.guest_agent.0'/>\n")
	b.WriteString("    </channel>\n")
	b.WriteString("    <console type='pty'/>\n")
	b.WriteString("    <graphics type='vnc' autoport='yes' listen='127.0.0.1'/>\n")
	b.WriteString("    <video><model type='vga'/></video>\n")
	b.WriteString("  </devices>\n")
	b.WriteString("</domain>\n")
	return b.String()
}

func sanitiseName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 24 {
		out = strings.Trim(out[:24], "-")
	}
	if out == "" {
		return "vm"
	}
	return out
}
