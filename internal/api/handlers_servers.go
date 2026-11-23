package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/events"
	"github.com/Variel42k/ovirt-backup/internal/libvirtx"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/proxmox"
)

// serverPayload описывает тело запроса для создания или изменения подключения. Отдельный
// тип позволяет добавлять внутренние поля без изменения публичного API и обновлять
// подключение без повторного ввода пароля.
type serverPayload struct {
	// ID заполняется только пробой из формы редактирования: по нему берётся
	// сохранённый секрет. Поиск по имени для этого не годится — оператор мог
	// заодно переименовать подключение, и тогда проба ушла бы с пустым паролем,
	// а оператор увидел бы «доступ запрещён» вместо своей опечатки в имени.
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	EngineURL string `json:"engine_url"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	// ClearPassword полезно только для мигрирующего KVM соединения. Разъемы с поддержкой
	// API по-прежнему требуют своего пароля или секрета токена во время проверки.
	ClearPassword bool   `json:"clear_password"`
	CACert        string `json:"ca_cert"`
	// ClearCACert нарочно отделяется от пустого CACert. API никогда не повторяет
	// сохраненный сертификат, поэтому форма редактирования представляет пустое значение,
	// когда оператор хочет сохранить существующий якорь доверия.
	ClearCACert bool     `json:"clear_ca_cert"`
	InsecureTLS bool     `json:"insecure_tls"`
	Enabled     *bool    `json:"enabled"`
	Tags        []string `json:"tags"`
	Notes       string   `json:"notes"`

	// Поля для подключений типа kvm.
	SSHHost       string `json:"ssh_host"`
	SSHPort       int    `json:"ssh_port"`
	SSHUsername   string `json:"ssh_username"`
	SSHPrivateKey string `json:"ssh_private_key"`
	// Сброс SSH-ключа запрашивается явно через ClearSSHPrivateKey. Пустое поле при
	// редактировании сохраняет действующий ключ.
	ClearSSHPrivateKey bool   `json:"clear_ssh_private_key"`
	SSHHostKey         string `json:"ssh_host_key"`
	ClearSSHHostKey    bool   `json:"clear_ssh_host_key"`
	// SSHTrustAnyHostKey — явный отказ проверять подлинность гипервизора.
	SSHTrustAnyHostKey bool   `json:"ssh_trust_any_host_key"`
	ScratchDir         string `json:"scratch_dir"`
	// FleecingStorage — хранилище узлов Proxmox для fleecing; пусто — без него.
	FleecingStorage string `json:"fleecing_storage"`
}

func (p serverPayload) apply(dst *model.Server) {
	dst.Name = p.Name
	dst.EngineURL = p.EngineURL
	dst.Username = p.Username
	if p.Password != "" || p.ClearPassword {
		dst.Password = p.Password
	}
	dst.ClearPassword = p.ClearPassword
	if p.CACert != "" || p.ClearCACert {
		dst.CACert = p.CACert
	}
	dst.InsecureTLS = p.InsecureTLS
	dst.Tags = p.Tags
	dst.Notes = p.Notes
	dst.SSHHost = p.SSHHost
	dst.SSHUsername = p.SSHUsername
	dst.SSHPrivateKey = p.SSHPrivateKey
	dst.ClearSSHPrivateKey = p.ClearSSHPrivateKey
	if p.SSHHostKey != "" || p.ClearSSHHostKey {
		dst.SSHHostKey = p.SSHHostKey
	}
	dst.SSHTrustAnyHostKey = p.SSHTrustAnyHostKey
	dst.ScratchDir = p.ScratchDir
	dst.FleecingStorage = strings.TrimSpace(p.FleecingStorage)
	if p.SSHPort > 0 {
		dst.SSHPort = p.SSHPort
	}
	if p.Kind != "" {
		dst.Kind = model.ServerKind(p.Kind)
	}
	if p.Enabled != nil {
		dst.Enabled = *p.Enabled
	}
}

// validateServer применяет собственные правила модели, которые различаются по типу
// соединения, и добавляет одно правило, которое имеет значение только при создании:
// совершенно новое соединение не имеет хранимого секрета, на который можно вернуться.
func validateServer(srv *model.Server, isNew bool) error {
	if err := srv.Validate(); err != nil {
		return badRequest("%v", err)
	}
	switch {
	case srv.Kind.UsesOVirtAPI():
		if _, err := ovirt.New(ovirt.Config{
			EngineURL: srv.EngineURL, CACert: srv.CACert, InsecureTLS: srv.InsecureTLS,
		}); err != nil {
			return badRequest("%v", err)
		}
	case srv.Kind.UsesProxmoxAPI():
		if _, err := proxmox.New(proxmox.Config{
			BaseURL: srv.EngineURL, TokenID: srv.Username, TokenSecret: srv.Password,
			CACert: srv.CACert, InsecureTLS: srv.InsecureTLS,
		}); err != nil {
			return badRequest("%v", err)
		}
		if srv.HasProxmoxDataPlane() {
			if _, err := proxmox.NewDataPlane(srv, 30*time.Second); err != nil {
				return badRequest("%v", err)
			}
		}
		// Тем же правилом проверяет помощник на узле: иначе ошибка всплыла бы
		// только ночью, на первом бэкапе.
		if srv.FleecingStorage != "" && !proxmox.ValidStorageID(srv.FleecingStorage) {
			return badRequest("недопустимый идентификатор хранилища для fleecing: %q", srv.FleecingStorage)
		}
	}
	// Новое подключение к гипервизору заводится только по ключу.
	//
	// Пароль для автоматических заданий означает, что он лежит в базе в
	// расшифровываемом виде и предъявляется каждому хосту при каждом
	// подключении. Ключ этим свойством не обладает: хост видит подпись, а не
	// секрет.
	//
	// Требование только к новым: уже заведённые подключения продолжают
	// работать, иначе обновление службы остановило бы ночное копирование. Они
	// помечены в списке, и переводить их на ключи нужно, но не в три часа ночи.
	if isNew && srv.Kind.UsesLibvirt() && srv.SSHPrivateKey == "" {
		return badRequest("для подключения к гипервизору нужен приватный ключ: " +
			"пароль в автоматических заданиях хранится расшифровываемым и " +
			"предъявляется хосту при каждом подключении")
	}
	return nil
}

func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) {
	servers, err := s.store.ListServers(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, servers)
}

func (s *Server) handleGetServer(w http.ResponseWriter, r *http.Request) {
	srv, err := s.store.GetServer(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, srv)
}

func (s *Server) handleCreateServer(w http.ResponseWriter, r *http.Request) {
	var payload serverPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}

	srv := &model.Server{Enabled: true, Kind: model.KindOVirt, SSHPort: 22}
	payload.apply(srv)
	if err := validateServer(srv, true); err != nil {
		s.writeError(w, r, err)
		return
	}

	if err := s.store.CreateServer(r.Context(), srv); err != nil {
		s.audit(r, "server.create", model.ScopeServer, srv.Name, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "server.create", model.ScopeServer, srv.ID, true, srv.Name)
	s.auditHostKeyTrust(r, model.ScopeServer, srv.ID, srv.Name, srv.SSHTrustAnyHostKey,
		srv.Kind.UsesLibvirt() || srv.HasProxmoxDataPlane())

	// Проверяйте сразу, чтобы оператор увидел, работает ли соединение, а не ждал следующего
	// опроса.
	go s.refreshServer(context.WithoutCancel(r.Context()), srv.ID)

	writeJSON(w, http.StatusCreated, srv)
}

func (s *Server) handleUpdateServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.store.GetServer(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	var payload serverPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	if payload.Kind != "" && model.ServerKind(payload.Kind) != existing.Kind {
		s.writeError(w, r, badRequest("тип существующего подключения менять нельзя; создайте новое подключение"))
		return
	}
	// apply сохраняет пароль, если запрос его явно не меняет и не удаляет.
	// Перед проверкой возвращаем сохранённый SSHPrivateKey, если форма
	// редактирования оставила поле пустым.
	storedKey := existing.SSHPrivateKey
	payload.apply(existing)
	if existing.SSHPrivateKey == "" && !payload.ClearSSHPrivateKey {
		existing.SSHPrivateKey = storedKey
	}

	if err := validateServer(existing, false); err != nil {
		s.writeError(w, r, err)
		return
	}

	if err := s.store.UpdateServer(r.Context(), existing); err != nil {
		s.audit(r, "server.update", model.ScopeServer, id, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	// Кэшированный клиент содержит старые учётные данные аутентификации и TLS настройки.
	s.pool.Invalidate(id)
	if s.proxmox != nil {
		s.proxmox.Invalidate(id)
	}
	s.audit(r, "server.update", model.ScopeServer, id, true, existing.Name)
	s.auditHostKeyTrust(r, model.ScopeServer, id, existing.Name, existing.SSHTrustAnyHostKey,
		existing.Kind.UsesLibvirt() || existing.HasProxmoxDataPlane())

	go s.refreshServer(context.WithoutCancel(r.Context()), id)
	writeJSON(w, http.StatusOK, existing)
}

func (s *Server) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	storages, err := s.store.ListStorageTargets(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	for _, target := range storages {
		if target.Kind == model.StorageDomainRepository && target.ServerID == id {
			if target.Automatic {
				count, countErr := s.store.CountRunsOnTarget(r.Context(), target.ID)
				if countErr != nil {
					s.writeError(w, r, countErr)
					return
				}
				jobs, jobErr := s.store.JobsOnTarget(r.Context(), target.ID)
				if jobErr != nil {
					s.writeError(w, r, jobErr)
					return
				}
				if count == 0 && len(jobs) == 0 {
					continue
				}
				s.writeError(w, r, badRequest("домен «%s» используется бэкапами или заданиями; перенесите копии и измените назначения перед удалением подключения", target.Name))
				return
			}
			s.writeError(w, r, badRequest("подключение используется хранилищем «%s» на дисковом домене; сначала удалите или перенастройте хранилище", target.Name))
			return
		}
	}
	if err := s.store.DeleteServer(r.Context(), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.pool.Invalidate(id)
	if s.proxmox != nil {
		s.proxmox.Invalidate(id)
	}
	s.audit(r, "server.delete", model.ScopeServer, id, true, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// probeResult содержит результат проверки подключения.
type probeResult struct {
	OK              bool   `json:"ok"`
	Error           string `json:"error,omitempty"`
	ProductName     string `json:"product_name,omitempty"`
	Version         string `json:"version,omitempty"`
	SupportsCBT     bool   `json:"supports_cbt"`
	SupportsBackup  bool   `json:"supports_backup"`
	SupportsRestore bool   `json:"supports_restore"`
	Clusters        int    `json:"clusters"`
	Hosts           int    `json:"hosts"`
	VMs             int    `json:"vms"`
	Latency         string `json:"latency,omitempty"`
	// Hint даёт понятное объяснение частым ошибкам вместо текста от библиотеки.
	Hint string `json:"hint,omitempty"`
	// ExcessPrivileges перечисляет то, что эта учётная запись может сверх
	// нужного резервному копированию. Заполняется фактической проверкой, а не
	// догадкой по имени: административную запись называют как угодно.
	//
	// Подключение с такой записью не запрещается — бывает, что завести
	// отдельную негде и некогда. Но оператор должен увидеть, чем платит:
	// пароль ляжет в базу службы и будет годиться для любых действий с
	// виртуализацией, включая те, которых у самой службы в интерфейсе нет.
	ExcessPrivileges []ovirt.ExcessPrivilege `json:"excess_privileges,omitempty"`
}

// handleProbeServer проверяет соединение, не сохраняя его, поэтому оператор может получить
// учётные данные аутентификации прямо перед их фиксацией.
func (s *Server) handleProbeServer(w http.ResponseWriter, r *http.Request) {
	var payload serverPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	// Пустые секреты в форме редактирования означают, что используются сохранённые значения.
	// Для проверки подставляем их из подключения по идентификатору. При создании формы без
	// идентификатора ищем сохранённое подключение по имени.
	if (payload.Password == "" && !payload.ClearPassword) ||
		(payload.SSHPrivateKey == "" && !payload.ClearSSHPrivateKey) ||
		(payload.CACert == "" && !payload.ClearCACert) ||
		(payload.SSHHostKey == "" && !payload.ClearSSHHostKey) {
		var existing *model.Server
		var err error
		switch {
		case payload.ID != "":
			existing, err = s.store.GetServer(r.Context(), payload.ID)
		case payload.Name != "":
			existing, err = s.store.GetServerByName(r.Context(), payload.Name)
		}
		if err == nil && existing != nil {
			payload.fillHiddenFrom(existing)
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	kind := model.ServerKind(payload.Kind)
	if kind == "" {
		kind = model.KindOVirt
	}
	if kind.UsesLibvirt() {
		writeJSON(w, http.StatusOK, s.probeLibvirt(ctx, payload))
		return
	}
	if kind.UsesProxmoxAPI() {
		writeJSON(w, http.StatusOK, s.probeProxmox(ctx, payload))
		return
	}
	if !kind.UsesOVirtAPI() {
		s.writeError(w, r, badRequest("неподдерживаемый тип системы виртуализации %q", kind))
		return
	}

	if payload.EngineURL == "" || payload.Username == "" {
		s.writeError(w, r, badRequest("нужны адрес движка и имя пользователя"))
		return
	}

	client, err := ovirt.New(ovirt.Config{
		EngineURL:   payload.EngineURL,
		Username:    payload.Username,
		Password:    payload.Password,
		CACert:      payload.CACert,
		InsecureTLS: payload.InsecureTLS,
		Timeout:     25 * time.Second,
		Logger:      s.log,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, probeResult{OK: false, Error: err.Error()})
		return
	}
	defer client.Logout(context.WithoutCancel(ctx))

	started := time.Now()
	info, err := client.Info(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, probeResult{OK: false, Error: err.Error(), Hint: probeHint(err)})
		return
	}
	clusters, err := client.ListClusters(ctx, "")
	if err != nil {
		writeJSON(w, http.StatusOK, probeResult{OK: false, Error: "не удалось прочитать кластеры: " + err.Error(), Hint: probeHint(err)})
		return
	}

	writeJSON(w, http.StatusOK, probeResult{
		OK:              true,
		ProductName:     info.ProductInfo.Name,
		Version:         info.Version(),
		SupportsCBT:     info.SupportsIncrementalBackup(),
		SupportsBackup:  true,
		SupportsRestore: true,
		Clusters:        len(clusters),
		Hosts:           info.Summary.Hosts.Total.Int(),
		VMs:             info.Summary.VMs.Total.Int(),
		Latency:         time.Since(started).Round(time.Millisecond).String(),
		// Проверка прав идёт после успешного входа и только чтением. Она не
		// мешает подключиться — её дело показать оператору, что учётная запись
		// может больше, чем нужно, пока он ещё не нажал «Сохранить».
		ExcessPrivileges: client.CheckExcessPrivileges(ctx),
	})
}

func (s *Server) probeProxmox(ctx context.Context, payload serverPayload) probeResult {
	if payload.EngineURL == "" || payload.Username == "" || payload.Password == "" {
		return probeResult{OK: false, Error: "нужны адрес Proxmox, API token ID и secret токена"}
	}
	client, err := proxmox.New(proxmox.Config{
		BaseURL: payload.EngineURL, TokenID: payload.Username, TokenSecret: payload.Password,
		CACert: payload.CACert, InsecureTLS: payload.InsecureTLS, Timeout: 25 * time.Second,
	})
	if err != nil {
		return probeResult{OK: false, Error: err.Error()}
	}
	started := time.Now()
	inv, err := client.FetchInventory(ctx, "")
	if err != nil {
		return probeResult{OK: false, Error: err.Error(), Hint: proxmoxHint(err)}
	}
	hint := "Инвентарь и управление ВМ доступны. Для бэкапа настройте отдельный SSH-канал данных и закрепите ключи всех узлов."
	backupReady := false
	dataServer := &model.Server{Kind: model.KindProxmox, SSHUsername: payload.SSHUsername,
		SSHPort: payload.SSHPort, SSHPrivateKey: payload.SSHPrivateKey, SSHHostKey: payload.SSHHostKey,
		SSHTrustAnyHostKey: payload.SSHTrustAnyHostKey}
	if dataServer.HasProxmoxDataPlane() {
		plane, planeErr := proxmox.NewDataPlane(dataServer, 20*time.Second)
		if len(inv.Hosts) == 0 {
			hint = "API доступен, но Proxmox не вернул ни одного узла; канал данных проверить невозможно."
		} else if planeErr == nil {
			backupReady = true
			for _, host := range inv.Hosts {
				address := host.Address
				if address == "" {
					address = host.Name
				}
				if _, probeErr := plane.Probe(ctx, address); probeErr != nil {
					backupReady = false
					hint = fmt.Sprintf("API доступен, но канал данных узла %s не готов: %v", host.Name, probeErr)
					break
				}
			}
		} else {
			hint = planeErr.Error()
		}
		if backupReady {
			hint = "Нативный полный бэкап и восстановление QEMU/LXC доступны на всех узлах кластера."
		}
	}
	if inv.Info.Clustered && !inv.Info.Quorate {
		hint = "Кластер ответил, но не имеет кворума. " + hint
	}
	return probeResult{OK: true, ProductName: "Proxmox VE", Version: inv.Info.FullVersion(),
		SupportsCBT: false, SupportsBackup: backupReady, SupportsRestore: backupReady,
		Clusters: len(inv.Clusters), Hosts: len(inv.Hosts), VMs: len(inv.VMs),
		Latency: time.Since(started).Round(time.Millisecond).String(), Hint: hint}
}

func proxmoxHint(err error) string {
	text := err.Error()
	switch {
	case containsAny(text, "401", "403", "permission", "authentication"):
		return "Proxmox отверг API-токен или ему не хватает прав Sys.Audit, VM.Audit и Datastore.Audit."
	case containsAny(text, "certificate", "x509", "tls"):
		return "Сертификат Proxmox не проверяется. Получите сертификат, сверьте SHA-256 и сохраните его."
	case containsAny(text, "no such host", "lookup"):
		return "Имя узла Proxmox не разрешается на сервере приложения. Проверьте DNS."
	case containsAny(text, "connection refused", "timeout", "deadline"):
		return "API Proxmox не отвечает. Обычно pveproxy доступен по HTTPS на порту 8006."
	default:
		return ""
	}
}

func (p *serverPayload) fillHiddenFrom(existing *model.Server) {
	if p.Password == "" && !p.ClearPassword {
		p.Password = existing.Password
	}
	if p.SSHPrivateKey == "" && !p.ClearSSHPrivateKey {
		p.SSHPrivateKey = existing.SSHPrivateKey
	}
	if p.CACert == "" && !p.ClearCACert {
		p.CACert = existing.CACert
	}
	if p.SSHHostKey == "" && !p.ClearSSHHostKey {
		p.SSHHostKey = existing.SSHHostKey
	}
}

// probeLibvirt тестирует SSH соединение с голым libvirt узлом.
//
// Он проверяет больше, чем доступность: оператор, который может войти в систему, но чей
// пользователь не входит в группу libvirt, в противном случае обнаружит, что только тогда,
// когда первая ночная резервная копия выходит из строя.
func (s *Server) probeLibvirt(ctx context.Context, payload serverPayload) probeResult {
	if payload.SSHHost == "" || payload.Username == "" {
		return probeResult{OK: false, Error: "нужны адрес хоста и пользователь SSH"}
	}

	started := time.Now()
	conn, err := libvirtx.Connect(ctx, libvirtx.Config{
		Host:            payload.SSHHost,
		Port:            payload.SSHPort,
		User:            payload.Username,
		Password:        payload.Password,
		PrivateKey:      payload.SSHPrivateKey,
		HostKey:         payload.SSHHostKey,
		TrustAnyHostKey: payload.SSHTrustAnyHostKey,
		ConnectTimeout:  20 * time.Second,
	})
	if err != nil {
		return probeResult{OK: false, Error: err.Error(), Hint: libvirtHint(err)}
	}
	defer conn.Close()

	info, err := conn.HostInfo(ctx)
	if err != nil {
		return probeResult{OK: false, Error: err.Error()}
	}
	supported, version, err := conn.SupportsIncrementalBackup(ctx)
	if err != nil {
		return probeResult{OK: false, Error: err.Error()}
	}

	result := probeResult{
		OK:              true,
		ProductName:     "libvirt " + version,
		Version:         version,
		SupportsCBT:     supported,
		SupportsBackup:  true,
		SupportsRestore: true,
		Clusters:        0,
		Hosts:           1,
		VMs:             info.TotalVMs,
		Latency:         time.Since(started).Round(time.Millisecond).String(),
	}
	if !supported {
		result.Hint = "libvirt старше 6.0: инкрементальный бэкап недоступен, будут только полные копии."
	}

	// Временный рабочий каталог должен существовать и быть записываемым qemu перед первой
	// резервной копией, а не в 3 утра во время нее.
	scratch := payload.ScratchDir
	if scratch == "" {
		scratch = "/var/lib/libvirt/qemu"
	}
	if free, err := conn.PrepareScratchDir(ctx, scratch); err != nil {
		result.Hint = fmt.Sprintf("каталог %s недоступен для записи: %v", scratch, err)
	} else if free > 0 && free < 10<<30 {
		result.Hint = fmt.Sprintf("в каталоге %s свободно всего %.1f ГБ — "+
			"scratch-файл растёт, пока идёт чтение бэкапа", scratch, float64(free)/(1<<30))
	}
	return result
}

func libvirtHint(err error) string {
	text := err.Error()
	switch {
	case containsAny(text, "unable to authenticate", "no supported methods"):
		return "SSH отверг учётные данные. Проверьте пользователя, пароль или приватный ключ."
	case containsAny(text, "libvirt-sock", "permission denied", "connection refused"):
		return "Сокет libvirt недоступен: проверьте, что libvirtd запущен и пользователь состоит в группе libvirt."
	case containsAny(text, "no such host", "lookup"):
		return "Имя KVM-хоста не разрешается. Проверьте DNS на сервере приложения и внутри контейнера; для корпоративного домена .local настройте unicast DNS route-domain."
	case containsAny(text, "knownhosts", "host key"):
		return "Ключ хоста не совпал с ожидаемым. Проверьте значение поля «ключ хоста»."
	case containsAny(text, "i/o timeout", "deadline"):
		return "Хост не отвечает на порту SSH. Проверьте доступность по сети."
	default:
		return ""
	}
}

// probeHint формулирует понятную подсказку для типичных ошибок первого подключения.
func probeHint(err error) string {
	switch {
	case ovirt.IsAuthError(err):
		return "Проверьте имя пользователя (обычно admin@internal или admin@ovirt@internalsso) и пароль."
	case containsAny(err.Error(), "certificate", "x509", "tls"):
		return "Сертификат движка не проверяется. Нажмите «Получить CA-сертификат» или включите режим без проверки TLS."
	case containsAny(err.Error(), "no such host", "lookup"):
		return "Имя движка не разрешается. Проверьте DNS на сервере приложения и внутри контейнера. Для корпоративного домена .local настройте unicast DNS route-domain; замена имени на IP может нарушить TLS."
	case containsAny(err.Error(), "connection refused", "timeout", "deadline"):
		return "Движок не отвечает на этом адресе и порту. Проверьте доступность по сети и что служба ovirt-engine запущена."
	default:
		return ""
	}
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if len(n) > 0 && len(haystack) >= len(n) {
			for i := 0; i+len(n) <= len(haystack); i++ {
				match := true
				for j := 0; j < len(n); j++ {
					a, b := haystack[i+j], n[j]
					if a >= 'A' && a <= 'Z' {
						a += 'a' - 'A'
					}
					if a != b {
						match = false
						break
					}
				}
				if match {
					return true
				}
			}
		}
	}
	return false
}

// handleFetchCA загружает сертификат движка CA, чтобы оператор мог просмотреть и принять
// его вместо того, чтобы отключать проверку вообще.
func (s *Server) handleFetchCA(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		EngineURL string `json:"engine_url"`
		Kind      string `json:"kind"`
	}
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	if payload.EngineURL == "" {
		s.writeError(w, r, badRequest("не указан адрес платформы виртуализации"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	var certPEM string
	var err error
	kind := model.ServerKind(payload.Kind)
	if kind.UsesProxmoxAPI() {
		certPEM, err = proxmox.FetchCertificateChain(ctx, payload.EngineURL, 15*time.Second)
	} else if kind == "" || kind.UsesOVirtAPI() {
		certPEM, err = ovirt.FetchCACert(ctx, payload.EngineURL, 15*time.Second)
	} else {
		s.writeError(w, r, badRequest("получение TLS-сертификата для %q не поддерживается", kind))
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	fingerprint, err := certificateFingerprint(certPEM)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"ca_cert":     certPEM,
		"fingerprint": fingerprint,
		"warning":     "Сертификат получен по непроверенному соединению. Сверьте SHA-256 на стороне платформы, прежде чем сохранять.",
	})
}

func certificateFingerprint(bundle string) (string, error) {
	block, _ := pem.Decode([]byte(bundle))
	if block == nil || block.Type != "CERTIFICATE" {
		return "", badRequest("движок не вернул сертификат PEM")
	}
	sum := sha256.Sum256(block.Bytes)
	raw := strings.ToUpper(hex.EncodeToString(sum[:]))
	parts := make([]string, 0, len(raw)/2)
	for len(raw) >= 2 {
		parts = append(parts, raw[:2])
		raw = raw[2:]
	}
	return strings.Join(parts, ":"), nil
}

func (s *Server) handleRefreshServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	srv, err := s.store.GetServer(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Monitor.Timeout)
	defer cancel()

	if err := s.monitor.PollServer(ctx, srv); err != nil {
		s.writeError(w, r, err)
		return
	}
	updated, err := s.store.GetServer(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// refreshServer опросы один движок в фоне, используется после создания/обновления.
func (s *Server) refreshServer(ctx context.Context, id string) {
	pollCtx, cancel := context.WithTimeout(ctx, s.cfg.Monitor.Timeout)
	defer cancel()

	srv, err := s.store.GetServer(pollCtx, id)
	if err != nil {
		return
	}
	if err := s.monitor.PollServer(pollCtx, srv); err != nil {
		s.log.Debug().Err(err).Str("сервер", srv.Name).Msg("первичный опрос не удался")
	}
	s.bus.Publish(events.Event{Kind: events.KindServerState, ServerID: id})
}

func (s *Server) handleServerSummary(w http.ResponseWriter, r *http.Request) {
	summary, err := s.buildSummary(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// buildSummary агрегатирует состояние одного движка для приборной панели.
func (s *Server) buildSummary(ctx context.Context, serverID string) (*model.ServerSummary, error) {
	srv, err := s.store.GetServer(ctx, serverID)
	if err != nil {
		return nil, err
	}
	summary := &model.ServerSummary{Server: srv}

	hosts, err := s.store.ListHosts(ctx, serverID)
	if err != nil {
		return nil, err
	}
	summary.HostsTotal = len(hosts)
	for _, h := range hosts {
		if h.HostHealthy() {
			summary.HostsUp++
		}
	}

	vms, err := s.store.ListVMs(ctx, serverID)
	if err != nil {
		return nil, err
	}
	summary.VMsTotal = len(vms)
	for _, vm := range vms {
		switch {
		case vm.Status == "paused":
			summary.VMsPaused++
		case vm.Running():
			summary.VMsUp++
		case vm.Status == "down":
			summary.VMsDown++
		}
	}

	domains, err := s.store.ListStorageDomains(ctx, serverID)
	if err != nil {
		return nil, err
	}
	for _, d := range domains {
		if d.Type != "" && d.Type != "data" {
			continue
		}
		summary.DomainsTotal++
		if d.Status == "active" || d.Status == "" {
			summary.DomainsActive++
		}
	}

	open, critical, err := s.store.CountOpenAlerts(ctx, serverID)
	if err != nil {
		return nil, err
	}
	summary.AlertsFiring, summary.AlertsCritical = open, critical

	if err := s.fillBackupStats(ctx, serverID, summary); err != nil {
		return nil, err
	}
	return summary, nil
}
