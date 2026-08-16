package ovirt

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// EngineConfigVersion — версия формата снимка. Меняется, когда меняется
// структура документа, а не его содержимое: разбирающий снимок должен уметь
// отличить старый формат от нового, не угадывая по набору полей.
const EngineConfigVersion = 1

// EngineConfig — снимок настроек движка.
//
// Копии дисков защищают данные, но не среду: домены хранения, сети, кластеры и
// шаблоны в них не входят. Восстановить ВМ на пустой движок — значит сначала
// заново собрать всё это по памяти, а память в такой момент подводит.
//
// Разделы хранятся сырым JSON движка, а не переложенными в свои структуры.
// Так снимок переживает и поля, о которых мы не подумали, и те, что появятся
// в новых версиях движка: своя структура молча теряет и то, и другое, а
// обнаруживается потеря в тот единственный день, когда снимок понадобился.
type EngineConfig struct {
	Version     int       `json:"version"`
	CollectedAt time.Time `json:"collected_at"`
	ServerID    string    `json:"server_id"`
	ServerName  string    `json:"server_name,omitempty"`
	Product     string    `json:"product,omitempty"`
	APIVersion  string    `json:"api_version,omitempty"`

	// Sections — раздел движка как есть: ключ это путь без ведущей косой.
	Sections map[string]json.RawMessage `json:"sections"`

	// Missing — разделы, которых движок не отдал, вместе с причиной.
	//
	// Отсутствие раздела не отменяет снимок: на старой версии часть путей
	// просто не существует, а часть закрыта правами учётной записи. Снимок из
	// одиннадцати разделов вместо двенадцати полезнее, чем отказ целиком.
	Missing map[string]string `json:"missing,omitempty"`
}

// engineConfigSections — что собирается и в каком порядке.
//
// Порядок — от общего к частному, чтобы читать документ можно было сверху
// вниз: сначала дата-центры и кластеры, потом сети и хранилища, потом права.
var engineConfigSections = []string{
	"datacenters",
	"clusters",
	"hosts",
	"networks",
	"vnicprofiles",
	"storagedomains",
	"diskprofiles",
	"templates",
	"macpools",
	"roles",
	"users",
	"groups",
	"permissions",
}

// CollectEngineConfig снимает настройки движка.
//
// Секретов в этих разделах движок не отдаёт: пароли подключения к хранилищам
// и учётные данные хостов доступны только на запись. Проверять снимок на них
// всё равно стоит при первом запуске на новой версии движка.
func (c *Client) CollectEngineConfig(ctx context.Context, serverID, serverName string) (*EngineConfig, error) {
	snapshot := &EngineConfig{
		Version:     EngineConfigVersion,
		CollectedAt: time.Now().UTC(),
		ServerID:    serverID,
		ServerName:  serverName,
		Sections:    make(map[string]json.RawMessage, len(engineConfigSections)),
	}

	// Версия движка идёт в документ отдельно: по ней потом понятно, на что
	// снимок вообще ложится обратно.
	if info, err := c.Info(ctx); err == nil {
		snapshot.Product = info.ProductInfo.Name
		snapshot.APIVersion = info.Version()
	}

	for _, section := range engineConfigSections {
		var raw json.RawMessage
		if err := c.get(ctx, "/"+section, &raw); err != nil {
			if snapshot.Missing == nil {
				snapshot.Missing = map[string]string{}
			}
			switch {
			case IsNotFound(err):
				snapshot.Missing[section] = "движок не знает такого раздела"
			case IsAuthError(err):
				snapshot.Missing[section] = "нет прав на чтение раздела"
			default:
				snapshot.Missing[section] = err.Error()
			}
			continue
		}
		snapshot.Sections[section] = raw
	}

	if len(snapshot.Sections) == 0 {
		return nil, fmt.Errorf("движок не отдал ни одного раздела настроек")
	}
	return snapshot, nil
}

// Encode превращает снимок в документ для хранилища.
//
// Отступы и порядок ключей заданы жёстко: снимок кладётся рядом с копиями и
// сравнивается со вчерашним, а документ, у которого порядок ключей пляшет от
// запуска к запуску, сравнивать нечем — различаться будет каждая строка.
func (s *EngineConfig) Encode() ([]byte, error) {
	ordered := struct {
		Version     int               `json:"version"`
		CollectedAt time.Time         `json:"collected_at"`
		ServerID    string            `json:"server_id"`
		ServerName  string            `json:"server_name,omitempty"`
		Product     string            `json:"product,omitempty"`
		APIVersion  string            `json:"api_version,omitempty"`
		Sections    map[string]any    `json:"sections"`
		Missing     map[string]string `json:"missing,omitempty"`
		SectionList []string          `json:"section_list"`
	}{
		Version:     s.Version,
		CollectedAt: s.CollectedAt,
		ServerID:    s.ServerID,
		ServerName:  s.ServerName,
		Product:     s.Product,
		APIVersion:  s.APIVersion,
		Sections:    make(map[string]any, len(s.Sections)),
		Missing:     s.Missing,
	}

	for name, raw := range s.Sections {
		// Разбираем и складываем обратно: encoding/json сортирует ключи карт
		// сам, и снимок становится сравнимым построчно.
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("раздел %s: %w", name, err)
		}
		ordered.Sections[name] = value
		ordered.SectionList = append(ordered.SectionList, name)
	}
	sort.Strings(ordered.SectionList)

	return json.MarshalIndent(ordered, "", "  ")
}
