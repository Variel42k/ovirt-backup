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

// serverPayload is the write shape of a connection. It is separate from the
// model so that adding an internal field does not silently become part of the
// public API, and so the password can be optional on update.
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
	// ClearPassword is only useful for a migrated KVM connection. API-backed
	// connectors still require their password or token secret during validation.
	ClearPassword bool   `json:"clear_password"`
	CACert        string `json:"ca_cert"`
	// ClearCACert is deliberately separate from an empty CACert. The API never
	// echoes a stored certificate, so an edit form submits an empty value when
	// the operator wants to keep the existing trust anchor.
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
	// ClearSSHPrivateKey is explicit because an empty write-only field means
	// "keep the stored key" while editing an existing connection.
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

// validateServer applies the model's own rules, which differ by connection
// type, and adds the one rule that only matters on creation: a brand new
// connection has no stored secret to fall back on.
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

	// Probe immediately so the operator sees whether the connection works
	// instead of waiting for the next poll.
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
	// apply keeps the write-only password unless the payload explicitly replaces
	// or clears it. SSHPrivateKey still uses the legacy empty-is-keep contract, so
	// restore it here before validation when the edit form leaves it blank.
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
	// The cached client holds the old credentials and TLS settings.
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

// probeResult is what the "проверить подключение" button gets back.
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

// handleProbeServer tests a connection without storing it, so an operator can
// get the credentials right before committing them.
func (s *Server) handleProbeServer(w http.ResponseWriter, r *http.Request) {
	var payload serverPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	// An edit form submits empty secrets and hidden trust material meaning
	// "unchanged"; resolve them from the stored connection so the probe tests
	// the actual connection. The id is the reliable key; the name is the
	// fallback for the create form, where there is no id yet but the operator
	// may be re-checking a saved one.
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

// probeLibvirt tests an SSH connection to a bare libvirt host.
//
// It checks more than reachability: an operator who can log in but whose user
// is not in the libvirt group will otherwise discover that only when the first
// nightly backup fails.
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

	// The scratch directory has to exist and be writable by qemu before the
	// first backup, not at 3am during it.
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

// probeHint translates the usual first-connection failures into an actionable
// sentence.
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

// handleFetchCA downloads the engine's CA certificate so the operator can
// review and accept it instead of disabling verification altogether.
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

// refreshServer polls one engine in the background, used after create/update.
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

// buildSummary aggregates one engine's state for the dashboard.
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
