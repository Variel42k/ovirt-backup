package backup

import (
	"context"
	"fmt"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

// Место под проверочную ВМ в движке oVirt.
//
// Проверка загрузкой через движок восстанавливает копию новой ВМ на выбранный
// домен хранения. Диски создаются тонкими, поэтому занимают столько, сколько в
// них данных, а не полный размер. Но домены часто заняты боевыми ВМ: если
// проверочная ВМ выберет место почти до конца, движок поставит на паузу все
// ВМ домена, которым понадобится расти. Поэтому, как и для горячего бэкапа,
// оставляется запас — DomainReserve.

// Вердикт по домену хранения для проверочной ВМ.
const (
	BootSpaceOK       = "ok"       // места хватает с запасом, даже на полный размер дисков
	BootSpaceTight    = "tight"    // данные поместятся, полный размер дисков — нет
	BootSpaceShort    = "short"    // данные не поместятся с запасом: проверка откажет
	BootSpaceInactive = "inactive" // домен не активен
	BootSpaceUnknown  = "unknown"  // движок не сообщил место или объём копии неизвестен
)

// BootDomainCheck — годится ли домен хранения для проверочной ВМ.
type BootDomainCheck struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Storage string `json:"storage,omitempty"`
	Status  string `json:"status,omitempty"`
	// Available и Total — свободное место и объём домена; -1 — неизвестно.
	Available int64 `json:"available"`
	Total     int64 `json:"total"`
	// Reserve — запас, который проверочная ВМ не трогает.
	Reserve int64 `json:"reserve"`
	// NeedData — сколько займут данные дисков; NeedFull — полный размер
	// дисков. -1 — неизвестно.
	NeedData int64  `json:"need_data"`
	NeedFull int64  `json:"need_full"`
	Verdict  string `json:"verdict"`
	Message  string `json:"message"`
}

// CheckBootDomain оценивает домен хранения для проверочной ВМ с данными
// needData и полным размером дисков needFull.
func CheckBootDomain(sd *model.StorageDomain, needData, needFull int64) BootDomainCheck {
	c := BootDomainCheck{ID: sd.ID, Name: sd.Name, Storage: sd.Storage, Status: sd.Status,
		Available: -1, Total: -1, NeedData: needData, NeedFull: needFull}
	total := sd.AvailableSize + sd.UsedSize
	if total > 0 {
		c.Available, c.Total = sd.AvailableSize, total
		c.Reserve = DomainReserve(total)
	}
	room := c.Available - c.Reserve

	switch {
	case sd.Status != "" && sd.Status != "active":
		c.Verdict = BootSpaceInactive
		c.Message = fmt.Sprintf("домен не активен (%s): диски проверочной ВМ на нём не создать", sd.Status)
	case c.Available < 0:
		c.Verdict = BootSpaceUnknown
		c.Message = "движок не сообщил свободное место домена — проверьте его сами"
	case needData < 0:
		c.Verdict = BootSpaceUnknown
		c.Message = fmt.Sprintf("объём копии неизвестен; свободно %s, запас %s",
			humanBytes(c.Available), humanBytes(c.Reserve))
	case needData > room:
		c.Verdict = BootSpaceShort
		c.Message = fmt.Sprintf("места не хватит: данные дисков займут около %s, а сверх запаса %s свободно %s. "+
			"Проверка не начнётся — выберите другой домен или освободите место",
			humanBytes(needData), humanBytes(c.Reserve), humanBytes(max(room, 0)))
	case needFull > room:
		c.Verdict = BootSpaceTight
		c.Message = fmt.Sprintf("места может не хватить: данные (около %s) помещаются, но полный размер "+
			"дисков %s больше свободного сверх запаса (%s). Тонкие диски растут, только когда гость пишет, "+
			"поэтому для пробного запуска обычно хватает — но домен окажется почти заполнен",
			humanBytes(needData), humanBytes(needFull), humanBytes(room))
	default:
		c.Verdict = BootSpaceOK
		c.Message = fmt.Sprintf("хватает: данные около %s, полный размер дисков %s, свободно сверх запаса %s",
			humanBytes(needData), humanBytes(needFull), humanBytes(room))
	}
	return c
}

// RestoreSizeEstimate — сколько места займут диски копии при восстановлении:
// data — чанки с данными (до этого дорастают тонкие диски), full — полный
// размер дисков.
func (e *Engine) RestoreSizeEstimate(ctx context.Context, runID, copyID string) (data, full int64, err error) {
	set, err := e.LoadVMChainCopy(ctx, runID, copyID)
	if err != nil {
		return 0, 0, err
	}
	defer set.Close()
	for _, id := range set.DiskOrder {
		reader, err := e.ReaderFor(set, id)
		if err != nil {
			return 0, 0, err
		}
		data += int64(reader.PresentChunks()) * reader.ChunkSize()
		full += reader.VirtualSize()
		reader.Close()
	}
	return min(data, full), full, nil
}

// Qcow2InitialSize — начальный размер тома тонкого qcow2 под data байт данных
// диска размером virtual: данные и метаданные qcow2 (таблицы L2 и refcount для
// кластеров по 64 КиБ) с запасом, как считает qemu-img measure. Больше полного
// размера qcow2 для этого диска не бывает — движок такой начальный размер не
// примет.
func Qcow2InitialSize(data, virtual int64) int64 {
	const (
		cluster = 64 << 10
		mib     = 1 << 20
		slack   = 256 << 20
	)
	if data < 0 {
		data = 0
	}
	if virtual > 0 && data > virtual {
		data = virtual
	}
	l2 := (virtual/cluster + 1) * 8
	refcount := ((virtual+l2)/cluster + 1) * 2
	meta := l2 + refcount + 16*cluster
	size := data + meta + slack
	if full := virtual + meta; virtual > 0 && size > full {
		size = full
	}
	return (size + mib - 1) / mib * mib
}

// OVirtClient — клиент движка oVirt из пула службы; нужен диспетчеру для
// проверки загрузкой через движок.
func (e *Engine) OVirtClient(srv *model.Server) (*ovirt.Client, error) {
	return e.pool.ForServer(srv)
}

// LiveStorageDomain — домен хранения по инвентарю, но со свободным местом,
// прочитанным у движка сейчас: инвентарь обновляется с задержкой, а решение
// перед созданием проверочной ВМ должно опираться на свежие цифры.
func (e *Engine) LiveStorageDomain(ctx context.Context, srv *model.Server, domainID string) (*model.StorageDomain, error) {
	var sd *model.StorageDomain
	if domains, err := e.store.ListStorageDomains(ctx, srv.ID); err == nil {
		for _, d := range domains {
			if d.ID == domainID {
				copied := *d
				sd = &copied
			}
		}
	}
	if client, err := e.pool.ForServer(srv); err == nil {
		if live, liveErr := client.GetStorageDomain(ctx, domainID); liveErr == nil {
			if sd == nil {
				sd = &model.StorageDomain{ID: domainID, Name: live.Name, Type: live.Type, Storage: live.Storage.Type}
			}
			if available, used := live.Available.Int64(), live.Used.Int64(); available+used > 0 {
				sd.AvailableSize, sd.UsedSize = available, used
			}
		}
	}
	if sd == nil {
		return nil, fmt.Errorf("домен хранения %s не найден у движка %s", domainID, srv.Name)
	}
	return sd, nil
}

// IsBlockStorage — блочный домен хранения oVirt: том raw на нём бывает только
// полным, а тонким — только qcow2.
func IsBlockStorage(storageType string) bool {
	return storageType == "iscsi" || storageType == "fcp"
}

// NewDiskLayout — формат и выделение нового диска oVirt, в который служба
// заливает сырой поток при восстановлении.
//
//   - Движок без Backup API (oVirt 4.3, imageio 1.x) пишет поток в файл тома
//     как есть, поэтому том должен быть raw: тонкий на файловом домене и
//     полный на блочном.
//   - С imageio 4.4+ формат может быть любым: сырой поток в qcow2 imageio
//     переводит сам. Поэтому формат исходного диска сохраняется, но на блочном
//     домене raw заменяется тонким qcow2 — тонкого raw там не бывает.
func NewDiskLayout(sourceFormat, storageType string, engineConverts bool) (format string, sparse bool) {
	block := IsBlockStorage(storageType)
	if !engineConverts {
		return "raw", !block
	}
	if block && sourceFormat != "cow" {
		return "cow", true
	}
	if sourceFormat == "" {
		sourceFormat = "raw"
	}
	return sourceFormat, true
}

// RestoreAllocatesFull — займёт ли восстановленный диск полный размер: raw на
// блочном домене движка без Backup API создаётся только полным.
func RestoreAllocatesFull(engineConverts bool, storageType string) bool {
	return !engineConverts && IsBlockStorage(storageType)
}

// domainStorageType — тип хранилища домена по инвентарю; пусто — неизвестен.
func (e *Engine) domainStorageType(ctx context.Context, serverID, domainID string) string {
	domains, err := e.store.ListStorageDomains(ctx, serverID)
	if err != nil {
		return ""
	}
	for _, d := range domains {
		if d.ID == domainID {
			return d.Storage
		}
	}
	return ""
}
