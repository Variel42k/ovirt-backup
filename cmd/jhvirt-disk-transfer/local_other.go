//go:build !linux

package main

import (
	"errors"
	"os"
	"time"
)

func (ui console) password(string) ([]byte, error) {
	return nil, errors.New("требуется Linux")
}
func lockDelivery(*os.File) error { return errors.New("требуется Linux") }

func rawFileIdentity(os.FileInfo) (uint64, time.Time, error) {
	return 0, time.Time{}, errors.New("требуется Linux")
}
