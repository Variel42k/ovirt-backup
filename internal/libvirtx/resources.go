package libvirtx

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"

	golibvirt "github.com/digitalocean/go-libvirt"
)

// StoragePoolInfo is the subset of a live libvirt pool needed by restore
// planning and by the existing storage-domain picker in the web client.
type StoragePoolInfo struct {
	Name       string
	Kind       string
	Capacity   int64
	Allocation int64
	Available  int64
	Active     bool
}

// NetworkInfo is a live libvirt virtual network.
type NetworkInfo struct {
	Name   string
	Active bool
}

// ManagedVolume is a volume created specifically for a restored VM.
type ManagedVolume struct {
	Handle     golibvirt.StorageVol
	Name       string
	Path       string
	DeviceType string // file | block
}

// ListStoragePools returns active pools. Inactive pools cannot accept a
// volume and therefore are deliberately not offered as restore targets.
func (c *Conn) ListStoragePools(ctx context.Context) ([]StoragePoolInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pools, _, err := c.lv.ConnectListAllStoragePools(1, golibvirt.ConnectListStoragePoolsActive)
	if err != nil {
		return nil, fmt.Errorf("список storage pool: %w", err)
	}
	out := make([]StoragePoolInfo, 0, len(pools))
	for _, pool := range pools {
		state, capacity, allocation, available, infoErr := c.lv.StoragePoolGetInfo(pool)
		if infoErr != nil {
			continue
		}
		kind := "pool"
		if raw, xmlErr := c.lv.StoragePoolGetXMLDesc(pool, 0); xmlErr == nil {
			var doc struct {
				Type string `xml:"type,attr"`
			}
			if xml.Unmarshal([]byte(raw), &doc) == nil && doc.Type != "" {
				kind = doc.Type
			}
		}
		out = append(out, StoragePoolInfo{
			Name: pool.Name, Kind: kind, Capacity: safeUint64(capacity),
			Allocation: safeUint64(allocation), Available: safeUint64(available),
			Active: state == uint8(golibvirt.StoragePoolRunning),
		})
	}
	return out, nil
}

// StoragePool looks up an active pool by name and reports its current space.
func (c *Conn) StoragePool(ctx context.Context, name string) (golibvirt.StoragePool, StoragePoolInfo, error) {
	if err := ctx.Err(); err != nil {
		return golibvirt.StoragePool{}, StoragePoolInfo{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return golibvirt.StoragePool{}, StoragePoolInfo{}, fmt.Errorf("не выбран storage pool")
	}
	pool, err := c.lv.StoragePoolLookupByName(name)
	if err != nil {
		return golibvirt.StoragePool{}, StoragePoolInfo{}, fmt.Errorf("storage pool %q не найден: %w", name, err)
	}
	state, capacity, allocation, available, err := c.lv.StoragePoolGetInfo(pool)
	if err != nil {
		return golibvirt.StoragePool{}, StoragePoolInfo{}, err
	}
	info := StoragePoolInfo{Name: pool.Name, Capacity: safeUint64(capacity),
		Allocation: safeUint64(allocation), Available: safeUint64(available),
		Active: state == uint8(golibvirt.StoragePoolRunning)}
	if !info.Active {
		return golibvirt.StoragePool{}, info, fmt.Errorf("storage pool %q не активен", name)
	}
	return pool, info, nil
}

// CreateStorageVolume allocates a sparse raw volume. The returned path comes
// from libvirt; caller-provided source paths are never inserted into domain
// XML or a remote shell command.
func (c *Conn) CreateStorageVolume(ctx context.Context, poolName, name string, capacity int64) (*ManagedVolume, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("размер тома должен быть больше нуля")
	}
	pool, _, err := c.StoragePool(ctx, poolName)
	if err != nil {
		return nil, err
	}
	name = safeVolumeName(name)
	if name == "" {
		return nil, fmt.Errorf("пустое имя тома")
	}
	doc := fmt.Sprintf("<volume><name>%s</name><capacity unit='bytes'>%d</capacity>"+
		"<allocation unit='bytes'>0</allocation><target><format type='raw'/>"+
		"<permissions><mode>0600</mode></permissions></target></volume>", xmlEscape(name), capacity)
	vol, err := c.lv.StorageVolCreateXML(pool, doc, 0)
	if err != nil {
		return nil, fmt.Errorf("создание тома %q: %w", name, err)
	}
	path, err := c.lv.StorageVolGetPath(vol)
	if err != nil {
		_ = c.lv.StorageVolDelete(vol, golibvirt.StorageVolDeleteNormal)
		return nil, fmt.Errorf("путь тома %q: %w", name, err)
	}
	volType, _, _, err := c.lv.StorageVolGetInfo(vol)
	if err != nil {
		_ = c.lv.StorageVolDelete(vol, golibvirt.StorageVolDeleteNormal)
		return nil, fmt.Errorf("тип тома %q: %w", name, err)
	}
	deviceType := "file"
	if golibvirt.StorageVolType(volType) == golibvirt.StorageVolBlock {
		deviceType = "block"
	}
	return &ManagedVolume{Handle: vol, Name: name, Path: path, DeviceType: deviceType}, nil
}

func (c *Conn) DeleteStorageVolume(ctx context.Context, volume *ManagedVolume) error {
	if volume == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.lv.StorageVolDelete(volume.Handle, golibvirt.StorageVolDeleteNormal); err != nil {
		return fmt.Errorf("удаление тома %q: %w", volume.Name, err)
	}
	return nil
}

func (c *Conn) ListNetworks(ctx context.Context) ([]NetworkInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	networks, _, err := c.lv.ConnectListAllNetworks(1, 0)
	if err != nil {
		return nil, fmt.Errorf("список сетей libvirt: %w", err)
	}
	out := make([]NetworkInfo, 0, len(networks))
	for _, network := range networks {
		active, activeErr := c.lv.NetworkIsActive(network)
		if activeErr == nil {
			out = append(out, NetworkInfo{Name: network.Name, Active: active != 0})
		}
	}
	return out, nil
}

func (c *Conn) NetworkExists(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	network, err := c.lv.NetworkLookupByName(strings.TrimSpace(name))
	if err != nil {
		return fmt.Errorf("сеть libvirt %q не найдена: %w", name, err)
	}
	active, err := c.lv.NetworkIsActive(network)
	if err != nil {
		return err
	}
	if active == 0 {
		return fmt.Errorf("сеть libvirt %q не активна", name)
	}
	return nil
}

func (c *Conn) BridgeExists(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("не указан bridge")
	}
	_, err := c.Run(ctx, "ip link show dev "+shellQuote(name)+" >/dev/null")
	if err != nil {
		return fmt.Errorf("bridge %q не найден: %w", name, err)
	}
	return nil
}

func safeVolumeName(value string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-.")
}

func xmlEscape(value string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(value))
	return b.String()
}

func safeUint64(value uint64) int64 {
	const maxInt64 = int64(^uint64(0) >> 1)
	if value > uint64(maxInt64) {
		return maxInt64
	}
	return int64(value)
}
