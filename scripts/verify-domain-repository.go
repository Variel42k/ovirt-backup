// Run with the service's configuration and environment, from its container:
// go run ./scripts/verify-domain-repository.go -config ... -server ... -domain ...
// Only creates and deletes disks in a unique smoke-test namespace. Does not
// change repository definitions, jobs, VMs, or the application's database.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/repo"
	"github.com/Variel42k/ovirt-backup/internal/secret"
	"github.com/Variel42k/ovirt-backup/internal/store"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "/app/config/ovirt-backup.yaml", "service config")
	serverID := flag.String("server", "", "existing engine connection ID")
	domainID := flag.String("domain", "", "existing active data domain ID")
	inspect := flag.Bool("inspect", false, "only list repository service disks on this domain")
	cleanupNamespace := flag.String("cleanup-namespace", "", "remove only disks from this exact smoke-UUID namespace")
	flag.Parse()
	if *serverID == "" || *domainID == "" {
		return fmt.Errorf("укажите -server и -domain")
	}
	if *cleanupNamespace != "" {
		if !strings.HasPrefix(*cleanupNamespace, "smoke-") {
			return fmt.Errorf("cleanup принимает только пространство smoke-UUID")
		}
		if _, err := uuid.Parse(strings.TrimPrefix(*cleanupNamespace, "smoke-")); err != nil {
			return err
		}
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	// A typo in a configuration path must never generate a replacement key.
	if cfg.Secrets.KeyBase64 == "" {
		if _, err = os.Stat(cfg.Secrets.KeyFile); err != nil {
			return err
		}
	}
	cipher, err := secret.NewFromConfig(cfg.Secrets)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	db, err := store.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	st := store.New(db, cipher)
	pool := ovirt.NewPool(st.GetServer, time.Minute, zerolog.Nop())
	defer pool.Close()
	repo.ConfigureStorageDomains(pool.Get, cfg.Backup.TempDir, cfg.Backup.Transfer.PreferProxy)
	target := &model.StorageTarget{Name: "domain smoke test", Kind: model.StorageDomainRepository, ServerID: *serverID,
		StorageDomainID: *domainID, Prefix: "smoke-" + uuid.NewString(), Enabled: true}
	client, err := pool.Get(ctx, *serverID)
	if err != nil {
		return err
	}
	domain, err := client.GetStorageDomain(ctx, *domainID)
	if err != nil {
		return err
	}
	if *inspect {
		disks, err := client.DomainDisks(ctx, *domainID)
		if err != nil {
			return err
		}
		for _, d := range disks {
			if strings.HasPrefix(d.Description, "jhvirt-repository") {
				fmt.Printf("id=%s size=%d status=%s description=%s\n", d.ID, d.ProvisionedSize.Int64(), d.Status, d.Description)
			}
		}
		return nil
	}
	if *cleanupNamespace != "" {
		disks, err := client.DomainDisks(ctx, *domainID)
		if err != nil {
			return err
		}
		for _, d := range disks {
			var meta struct {
				Namespace string `json:"ns"`
			}
			published := strings.HasPrefix(d.Description, "jhvirt-repository-v1:") && json.Unmarshal([]byte(strings.TrimPrefix(d.Description, "jhvirt-repository-v1:")), &meta) == nil && meta.Namespace == *cleanupNamespace
			pending := d.Description == "jhvirt-repository-pending:"+*cleanupNamespace || d.Description == "jhvirt-repository-data:"+*cleanupNamespace
			if !published && !pending {
				continue
			}
			vms, err := client.DiskVMs(ctx, d.ID)
			if err != nil {
				return err
			}
			if len(vms) > 0 {
				return fmt.Errorf("тестовый диск %s подключён к ВМ", d.ID)
			}
			if err = client.DeleteDiskWhenUnlocked(ctx, d.ID); err != nil {
				return err
			}
			fmt.Println("removed smoke disk", d.ID)
		}
		return nil
	}
	fmt.Printf("domain=%s storage=%s namespace=%s\n", domain.Name, domain.Storage.Type, target.Prefix)
	b, err := repo.Open(ctx, target)
	if err != nil {
		return err
	}
	defer func() { _ = b.Close() }()
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Minute)
		defer stop()
		if _, err := b.DeletePrefix(cleanup, ""); err != nil {
			fmt.Fprintln(os.Stderr, "cleanup:", err)
		}
	}()
	payload := make([]byte, 3*1024*1024+791)
	if _, err = rand.Read(payload); err != nil {
		return err
	}
	copy(payload, []byte{'Q', 'F', 'I', 0xfb})
	key := "jhvirt/smoke/disk.data"
	for _, size := range []int64{int64(len(payload)), -1} {
		if _, err = b.Put(ctx, key, bytes.NewReader(payload), size); err != nil {
			return fmt.Errorf("put (%d): %w", size, err)
		}
		fmt.Printf("put size=%d OK\n", size)
	}
	if _, err = b.Put(ctx, "jhvirt/smoke/run.json", bytes.NewBufferString(`{"smoke":true}`), 14); err != nil {
		return err
	}
	if err = b.Close(); err != nil {
		return err
	}
	b, err = repo.Open(ctx, target)
	if err != nil {
		return err
	}
	items, err := b.List(ctx, "jhvirt/")
	if err != nil || len(items) != 2 {
		return fmt.Errorf("list: count=%d err=%v", len(items), err)
	}
	fmt.Println("reopen and list OK")
	for _, span := range [][2]int64{{0, -1}, {507, 1001}, {int64(len(payload)) - 113, 113}} {
		r, err := b.GetRange(ctx, key, span[0], span[1])
		if err != nil {
			return err
		}
		got, readErr := io.ReadAll(r)
		_ = r.Close()
		if readErr != nil {
			return readErr
		}
		end := int64(len(payload))
		if span[1] >= 0 {
			end = span[0] + span[1]
		}
		if !bytes.Equal(got, payload[span[0]:end]) {
			return fmt.Errorf("range %v: wrong bytes", span)
		}
		fmt.Printf("read range=%v OK\n", span)
	}
	if _, err = b.DeletePrefix(ctx, "jhvirt/"); err != nil {
		return err
	}
	if items, err = b.List(ctx, ""); err != nil || len(items) != 0 {
		return fmt.Errorf("delete: count=%d err=%v", len(items), err)
	}
	fmt.Println("delete OK; no test objects remain")
	return nil
}
