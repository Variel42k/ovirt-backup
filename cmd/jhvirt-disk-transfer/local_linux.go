package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func (ui console) password(label string) ([]byte, error) {
	fd := int(os.Stdin.Fd())
	saved, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, errors.New("для ввода пароля нужен терминал (SSH с -t)")
	}
	hidden := *saved
	hidden.Lflag &^= unix.ECHO | unix.ECHONL
	hidden.Lflag |= unix.ICANON | unix.ISIG
	hidden.Iflag |= unix.ICRNL
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &hidden); err != nil {
		return nil, err
	}
	defer unix.IoctlSetTermios(fd, unix.TCSETS, saved)
	fmt.Fprint(ui.output, label, ": ")
	password, err := consoleRead(ui.ctx, func() ([]byte, error) { return ui.input.ReadBytes('\n') })
	fmt.Fprintln(ui.output)
	return bytes.TrimSuffix(password, []byte{'\n'}), err
}

func lockDelivery(file *os.File) error {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return errJobActive
	}
	return err
}

func rawFileIdentity(info os.FileInfo) (uint64, time.Time, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, time.Time{}, errors.New("не удалось получить идентификатор исходного тома")
	}
	return stat.Ino, time.Unix(stat.Ctim.Unix()), nil
}
