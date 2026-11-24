package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"
)

const sshOptions = "-o StrictHostKeyChecking=yes -o UpdateHostKeys=no -o ConnectTimeout=10 -o ServerAliveInterval=15 -o ServerAliveCountMax=3"

type sshPeer struct {
	socket string
	stop   func()
}

func (peer sshPeer) options() string {
	options := sshOptions + " -o BatchMode=yes"
	if peer.socket != "" {
		options += " -o ControlMaster=no -S " + peer.socket
	}
	return options
}

func (peer sshPeer) close() {
	if peer.stop != nil {
		peer.stop()
	}
}

func (peer sshPeer) connected(ctx context.Context, host string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "ssh", "-S", peer.socket, "-O", "check", "--", host).Run() == nil
}

func openSSHPeer(ctx context.Context, ui console, host string) (sshPeer, error) {
	password, err := chooseSSHPassword(ui, host)
	if err != nil || !password {
		return sshPeer{}, err
	}
	return openPasswordSSH(ctx, ui, host)
}

func chooseSSHPassword(ui console, host string) (bool, error) {
	for {
		ui.say("1 — SSH-ключ (Enter); 2 — пароль SSH.")
		answer, err := ui.ask("Способ входа на " + host)
		if err != nil {
			return false, err
		}
		switch answer {
		case "", "1":
			return false, nil
		case "2":
			return true, nil
		default:
			ui.say("Введите 1 или 2.")
		}
	}
}

func openPasswordSSH(ctx context.Context, ui console, host string) (sshPeer, error) {
	fd := int(os.Stdin.Fd())
	state, err := term.GetState(fd)
	if err != nil {
		return sshPeer{}, errors.New("для ввода пароля SSH нужен терминал (SSH с -t)")
	}
	defer term.Restore(fd, state)
	dir, err := os.MkdirTemp("/tmp", "jhvt-ssh-")
	if err != nil {
		return sshPeer{}, err
	}
	socket := filepath.Join(dir, "control")
	masterCtx, cancel := context.WithCancel(ctx)
	args := append(strings.Fields(sshOptions), "-M", "-S", socket, "-N", "-o", "ControlPersist=no",
		"-o", "BatchMode=no", "-o", "PreferredAuthentications=keyboard-interactive,password",
		"-o", "ForwardAgent=no", "-o", "ForwardX11=no", "--", host)
	master := exec.CommandContext(masterCtx, "ssh", args...)
	master.Cancel = func() error { return master.Process.Signal(os.Interrupt) }
	master.WaitDelay = 3 * time.Second
	stderr := captureCommandErrors(master, ui.output)
	master.Stdin = os.Stdin
	if err := master.Start(); err != nil {
		cancel()
		os.Remove(dir)
		return sshPeer{}, err
	}
	var waitErr error
	exited := make(chan struct{})
	go func() {
		waitErr = master.Wait()
		close(exited)
	}()
	peer := sshPeer{socket: socket, stop: func() {
		cancel()
		<-exited
		os.Remove(socket)
		os.Remove(dir)
	}}
	// OpenSSH itself reads the password from the terminal and opens its control socket after authentication.
	// All later commands and rsync use that socket with BatchMode=yes, so they cannot prompt again.
	poll := time.NewTicker(150 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			peer.close()
			return sshPeer{}, ctx.Err()
		case <-exited:
			peer.close()
			if waitErr == nil {
				return sshPeer{}, errors.New("OpenSSH закрыл соединение до начала сеанса")
			}
			return sshPeer{}, fmt.Errorf("OpenSSH завершился до установки связи с %s: %w", host,
				transferFailure(waitErr, stderr.String(), true))
		case <-poll.C:
			if _, err := os.Stat(socket); err != nil {
				continue
			}
			if peer.connected(ctx, host) {
				return peer, nil
			}
		}
	}
}
