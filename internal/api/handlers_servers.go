package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"adveng/jh_virt/internal/events"
	"adveng/jh_virt/internal/libvirtx"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/ovirt"
)

// serverPayload is the write shape of a connection. It is separate from the
// model so that adding an internal field does not silently become part of the
// public API, and so the password can be optional on update.
type serverPayload struct {
	// ID заполняется только пробой из формы редактирования: по нему берётся
	// сохранённый секрет. Поиск по имени для этого не годится — оператор мог
	// заодно переименовать подключение, и тогда проба ушла бы с пустым паролем,
	// а оператор увидел бы «доступ запрещён» вместо своей опечатки в имени.
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	EngineURL   string   `json:"engine_url"`
	Username    string   `json:"username"`
	Password    string   `json:"password"`
	CACert      string   `json:"ca_cert"`
	InsecureTLS bool     `json:"insecure_tls"`
	Enabled     *bool    `json:"enabled"`
	Tags        []string `json:"tags"`
	Notes       string   `json:"notes"`

	// Поля для подключений типа kvm.
	SSHHost       string `json:"ssh_host"`
	SSHPort       int    `json:"ssh_port"`
	SSHPrivateKey string `json:"ssh_private_key"`
	SSHHostKey    string `json:"ssh_host_key"`
	ScratchDir    string `json:"scratch_dir"`
}

func (p serverPayload) apply(dst *model.Server) {
	dst.Name = p.Name
	dst.EngineURL = p.EngineURL
	dst.Username = p.Username
	dst.Password = p.Password
	dst.CACert = p.CACert
	dst.InsecureTLS = p.InsecureTLS
	dst.Tags = p.Tags
	dst.Notes = p.Notes
	dst.SSHHost = p.SSHHost
	dst.SSHPrivateKey = p.SSHPrivateKey
	dst.SSHHostKey = p.SSHHostKey
	dst.ScratchDir = p.ScratchDir
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
	if isNew && srv.Password == "" && srv.SSHPrivateKey == "" {
		return badRequest("не указан пароль")
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
	// An empty password means "keep the stored one", which is what lets the UI
	// render an edit form without ever holding the secret.
	//
	// The substitution has to happen here and not only in the store, because
	// validation runs in between and looks at the secret: for a libvirt
	// connection it requires a password or a key, so a blanked field turned
	// "change the scratch directory" into "не указан пароль" and made editing a
	// KVM connection impossible without retyping the credentials.
	storedPassword, storedKey := existing.Password, existing.SSHPrivateKey
	payload.apply(existing)
	if existing.Password == "" {
		existing.Password = storedPassword
	}
	if existing.SSHPrivateKey == "" {
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
	s.audit(r, "server.update", model.ScopeServer, id, true, existing.Name)

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
	s.audit(r, "server.delete", model.ScopeServer, id, true, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// probeResult is what the "проверить подключение" button gets back.
type probeResult struct {
	OK          bool   `json:"ok"`
	Error       string `json:"error,omitempty"`
	ProductName string `json:"product_name,omitempty"`
	Version     string `json:"version,omitempty"`
	SupportsCBT bool   `json:"supports_cbt"`
	Hosts       int    `json:"hosts"`
	VMs         int    `json:"vms"`
	Latency     string `json:"latency,omitempty"`
	// Hint даёт понятное объяснение частым ошибкам вместо текста от библиотеки.
	Hint string `json:"hint,omitempty"`
}

// handleProbeServer tests a connection without storing it, so an operator can
// get the credentials right before committing them.
func (s *Server) handleProbeServer(w http.ResponseWriter, r *http.Request) {
	var payload serverPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	// An edit form submits empty secrets meaning "unchanged"; resolve them from
	// the stored connection so the probe tests the real credentials. The id is
	// the reliable key; the name is the fallback for the create form, where
	// there is no id yet but the operator may be re-checking a saved one.
	if payload.Password == "" || payload.SSHPrivateKey == "" {
		var existing *model.Server
		var err error
		switch {
		case payload.ID != "":
			existing, err = s.store.GetServer(r.Context(), payload.ID)
		case payload.Name != "":
			existing, err = s.store.GetServerByName(r.Context(), payload.Name)
		}
		if err == nil && existing != nil {
			if payload.Password == "" {
				payload.Password = existing.Password
			}
			if payload.SSHPrivateKey == "" {
				payload.SSHPrivateKey = existing.SSHPrivateKey
			}
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	if model.ServerKind(payload.Kind).UsesLibvirt() {
		writeJSON(w, http.StatusOK, s.probeLibvirt(ctx, payload))
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

	writeJSON(w, http.StatusOK, probeResult{
		OK:          true,
		ProductName: info.ProductInfo.Name,
		Version:     info.Version(),
		SupportsCBT: info.SupportsIncrementalBackup(),
		Hosts:       info.Summary.Hosts.Total.Int(),
		VMs:         info.Summary.VMs.Total.Int(),
		Latency:     time.Since(started).Round(time.Millisecond).String(),
	})
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
		Host:           payload.SSHHost,
		Port:           payload.SSHPort,
		User:           payload.Username,
		Password:       payload.Password,
		PrivateKey:     payload.SSHPrivateKey,
		HostKey:        payload.SSHHostKey,
		ConnectTimeout: 20 * time.Second,
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
		OK:          true,
		ProductName: "libvirt " + version,
		Version:     version,
		SupportsCBT: supported,
		Hosts:       1,
		VMs:         info.TotalVMs,
		Latency:     time.Since(started).Round(time.Millisecond).String(),
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
		return "Имя хоста не разрешается в адрес. Проверьте DNS или укажите IP."
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
		return "Имя движка не разрешается в адрес. Проверьте DNS или укажите IP."
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
	}
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	if payload.EngineURL == "" {
		s.writeError(w, r, badRequest("не указан адрес движка"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	pem, err := ovirt.FetchCACert(ctx, payload.EngineURL, 15*time.Second)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"ca_cert": pem,
		"warning": "Сертификат получен по непроверенному соединению. Сверьте отпечаток с тем, что показывает движок, прежде чем сохранять.",
	})
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
