package ovirt

import (
	"context"
	"fmt"
	"time"
)

// DomainDisks includes detached repository disks, which VM inventory omits.
func (c *Client) DomainDisks(ctx context.Context, domainID string) ([]Disk, error) {
	var list struct {
		Disks []Disk `json:"disk"`
	}
	err := c.get(ctx, "/storagedomains/"+domainID+"/disks", &list)
	return list.Disks, err
}

// An image transfer can be terminal while the engine's command still holds
// its internal disk lock. disk.status alone does not reveal that lock.
func (c *Client) DeleteDiskWhenUnlocked(ctx context.Context, diskID string) error {
	err := retryRepositoryDiskLock(ctx, 2*time.Minute, 2*time.Second, func() error { return c.DeleteDisk(ctx, diskID) })
	if err != nil {
		return err
	}
	return waitRepositoryDiskState(ctx, func(ctx context.Context) (bool, error) {
		_, err := c.GetDisk(ctx, diskID)
		if IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}

// Disk updates/deletions may be accepted before their asynchronous engine
// command commits. Do not publish a successful repository operation early.
func waitRepositoryDiskState(ctx context.Context, done func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		ok, err := done(ctx)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("ожидание сохранения состояния диска: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func retryRepositoryDiskLock(ctx context.Context, timeout, interval time.Duration, operation func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := operation()
		if err == nil || !IsLockConflict(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("движок не снял блокировку диска за %s: %w", timeout, err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
