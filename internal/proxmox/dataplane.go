package proxmox

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/sshtrust"
)

const (
	DataPlaneProtocol = "jhvirt-pve-data-plane/1"
	dataPlaneHelper   = "/usr/local/sbin/jhvirt-pve-data-plane"
)

var pveStorageID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidStorageID reports whether value is safe to pass as a Proxmox storage
// identifier. Keeping the same validation in plans and execution prevents a
// plan from becoming ready for an operation the forced-command helper rejects.
func ValidStorageID(value string) bool {
	return pveStorageID.MatchString(value)
}

// DataPlane is the SSH transport for bytes that the Proxmox REST API cannot
// return. The key should be restricted to dataPlaneHelper in authorized_keys;
// every command sent here is a fixed protocol operation with validated fields.
type DataPlane struct {
	user            string
	privateKey      string
	hostKeyCallback ssh.HostKeyCallback
	port            int
	timeout         time.Duration
}

func NewDataPlane(srv *model.Server, timeout time.Duration) (*DataPlane, error) {
	if srv == nil || !srv.Kind.UsesProxmoxAPI() {
		return nil, errors.New("не задано подключение Proxmox")
	}
	if !srv.HasProxmoxDataPlane() {
		return nil, errors.New("канал данных Proxmox не настроен: нужны пользователь, приватный ключ и закреплённые ключи всех узлов")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	port := srv.SSHPort
	if port == 0 {
		port = 22
	}
	hostKeyCallback, err := sshtrust.AddressBoundCallback(srv.SSHHostKey, srv.SSHTrustAnyHostKey)
	if err != nil {
		return nil, fmt.Errorf("ключи узлов Proxmox: %w", err)
	}
	return &DataPlane{user: strings.TrimSpace(srv.SSHUsername), privateKey: srv.SSHPrivateKey,
		hostKeyCallback: hostKeyCallback, port: port, timeout: timeout}, nil
}

// Probe verifies authentication, host pinning and the helper protocol without
// reading guest data or changing the node.
func (d *DataPlane) Probe(ctx context.Context, host string) error {
	var stdout bytes.Buffer
	if err := d.run(ctx, host, dataPlaneHelper+" probe", nil, &stdout); err != nil {
		return err
	}
	if strings.TrimSpace(stdout.String()) != DataPlaneProtocol {
		return fmt.Errorf("узел %s вернул несовместимый протокол канала данных %q", host, strings.TrimSpace(stdout.String()))
	}
	return nil
}

// Backup streams one native vzdump archive from the node that currently owns
// the guest. No temporary archive is created on the Proxmox storage.
func (d *DataPlane) Backup(ctx context.Context, host, vmID string, consume func(io.Reader) error) error {
	kind, numericID, err := ParseVMID(vmID)
	if err != nil {
		return err
	}
	return d.stream(ctx, host, fmt.Sprintf("%s backup %s %s", dataPlaneHelper, kind, numericID), consume)
}

// Restore feeds a native archive to qmrestore/pct restore. The helper assigns
// fresh MAC addresses and keeps restored NICs disconnected unless the operator
// explicitly selected attached networking.
func (d *DataPlane) Restore(ctx context.Context, host, kind, vmID, storage, name string,
	network model.RestoreVMNetwork, start bool, archive io.Reader) error {
	if kind != "qemu" && kind != "lxc" {
		return fmt.Errorf("неверный тип гостя Proxmox %q", kind)
	}
	if _, _, err := ParseVMID(kind + "/" + vmID); err != nil {
		return err
	}
	if !ValidStorageID(storage) {
		return fmt.Errorf("неверный идентификатор хранилища Proxmox %q", storage)
	}
	if network == "" {
		network = model.RestoreNetworkDetached
	}
	if network != model.RestoreNetworkDetached && network != model.RestoreNetworkAttached {
		return fmt.Errorf("неверный режим сети %q", network)
	}
	encodedName := base64.StdEncoding.EncodeToString([]byte(name))
	startFlag := "0"
	if start {
		startFlag = "1"
	}
	command := fmt.Sprintf("%s restore %s %s %s %s %s %s", dataPlaneHelper, kind, vmID,
		storage, network, startFlag, encodedName)
	return d.run(ctx, host, command, archive, io.Discard)
}

func (d *DataPlane) stream(ctx context.Context, host, command string, consume func(io.Reader) error) error {
	client, err := d.connect(ctx, host)
	if err != nil {
		return err
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("открытие SSH-сессии на %s: %w", host, err)
	}
	defer session.Close()
	stdout, err := session.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := &limitedBuffer{limit: 64 << 10}
	session.Stderr = stderr
	if err := session.Start(command); err != nil {
		return fmt.Errorf("запуск канала данных на %s: %w", host, err)
	}
	cancelWatch := make(chan struct{})
	defer close(cancelWatch)
	go func() {
		select {
		case <-ctx.Done():
			_ = client.Close()
		case <-cancelWatch:
		}
	}()
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	consumeErr := consume(stdout)
	if consumeErr != nil {
		_ = client.Close()
		<-done
		return consumeErr
	}
	select {
	case err := <-done:
		return commandError(host, err, stderr.String())
	case <-ctx.Done():
		_ = client.Close()
		<-done
		return ctx.Err()
	}
}

func (d *DataPlane) run(ctx context.Context, host, command string, stdin io.Reader, stdout io.Writer) error {
	client, err := d.connect(ctx, host)
	if err != nil {
		return err
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("открытие SSH-сессии на %s: %w", host, err)
	}
	defer session.Close()
	stderr := &limitedBuffer{limit: 64 << 10}
	session.Stdin, session.Stdout, session.Stderr = stdin, stdout, stderr
	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()
	select {
	case err := <-done:
		return commandError(host, err, stderr.String())
	case <-ctx.Done():
		_ = client.Close()
		<-done
		return ctx.Err()
	}
}

func (d *DataPlane) connect(ctx context.Context, host string) (*ssh.Client, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil, errors.New("у узла Proxmox нет адреса")
	}
	signer, err := ssh.ParsePrivateKey([]byte(d.privateKey))
	if err != nil {
		return nil, fmt.Errorf("разбор приватного ключа канала данных: %w", err)
	}
	addr := net.JoinHostPort(host, fmt.Sprint(d.port))
	raw, err := (&net.Dialer{Timeout: d.timeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("SSH к узлу Proxmox %s: %w", addr, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	} else {
		_ = raw.SetDeadline(time.Now().Add(d.timeout))
	}
	conn, channels, requests, err := ssh.NewClientConn(raw, addr, &ssh.ClientConfig{
		User: d.user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: d.hostKeyCallback, Timeout: d.timeout,
	})
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("SSH-аутентификация на %s: %w", addr, err)
	}
	_ = raw.SetDeadline(time.Time{})
	return ssh.NewClient(conn, channels, requests), nil
}

func commandError(host string, err error, stderr string) error {
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(stderr)
	if detail == "" {
		return fmt.Errorf("канал данных Proxmox на %s: %w", host, err)
	}
	return fmt.Errorf("канал данных Proxmox на %s: %w: %s", host, err, detail)
}

type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	if remaining := b.limit - b.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buf.Write(p)
	}
	return original, nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }
