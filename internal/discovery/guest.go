package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/sshpool"
	"github.com/Variel42k/ovirt-backup/internal/sshstats"
)

type guestReport struct {
	Hostname   string `json:"hostname"`
	Containers []struct {
		Name  string `json:"name"`
		Image string `json:"image"`
	} `json:"containers"`
	Services []struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Exec        string   `json:"exec"`
		DataPaths   []string `json:"data_paths"`
	} `json:"services"`
	Mounts []struct {
		Source string `json:"source"`
		Target string `json:"target"`
		Type   string `json:"type"`
	} `json:"mounts"`
	Cron []string `json:"cron"`
}

type guestProbe struct {
	cfg     config.DiscoveryGuestConfig
	signer  ssh.Signer
	hostKey ssh.HostKeyCallback
	poolKey string
}

func newGuestProbe(cfg config.DiscoveryGuestConfig) (*guestProbe, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	key, err := os.ReadFile(cfg.PrivateKeyFile)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("ключ discovery: %w", err)
	}
	callback, err := knownhosts.New(cfg.KnownHostsFile)
	if err != nil {
		return nil, fmt.Errorf("known_hosts discovery: %w", err)
	}
	return &guestProbe{cfg: cfg, signer: signer, hostKey: callback, poolKey: sshpool.Key(string(key), cfg.KnownHostsFile, cfg.Username)}, nil
}

func (p *guestProbe) probe(ctx context.Context, address string) (*guestReport, error) {
	port := p.cfg.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(address, fmt.Sprint(port))
	key := sshpool.Key("guest-discovery", p.poolKey, addr)
	session, done, err := sshpool.Shared().Session(ctx, key, func(ctx context.Context) (*ssh.Client, error) {
		timeout := p.cfg.Timeout
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		raw, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		conn, chans, reqs, err := ssh.NewClientConn(raw, addr, &ssh.ClientConfig{User: p.cfg.Username, Auth: []ssh.AuthMethod{ssh.PublicKeys(p.signer)}, HostKeyCallback: p.hostKey, Timeout: timeout})
		sshstats.Record(addr, sshstats.Discovery, err)
		if err != nil {
			_ = raw.Close()
			return nil, err
		}
		return ssh.NewClient(conn, chans, reqs), nil
	})
	if err != nil {
		return nil, err
	}
	broken := false
	defer func() { done(broken) }()
	timeout := p.cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	opCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type commandResult struct {
		out []byte
		err error
	}
	doneCommand := make(chan commandResult, 1)
	go func() {
		out, err := session.Output("/usr/local/sbin/jhvirt-discovery probe")
		doneCommand <- commandResult{out: out, err: err}
	}()
	var out []byte
	select {
	case result := <-doneCommand:
		if result.err != nil {
			return nil, result.err
		}
		out = result.out
	case <-opCtx.Done():
		broken = true
		_ = session.Close()
		return nil, opCtx.Err()
	}
	if len(out) > 2<<20 {
		broken = true
		return nil, fmt.Errorf("ответ helper больше 2 МиБ")
	}
	var report guestReport
	if err := json.Unmarshal(out, &report); err != nil {
		return nil, fmt.Errorf("ответ helper: %w", err)
	}
	report.Hostname = strings.TrimSpace(report.Hostname)
	return &report, nil
}
