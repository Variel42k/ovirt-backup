package api

import (
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"

	"adveng/jh_virt/internal/backup"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/store"
)

// optionDescriptor is a machine-readable enum entry with a human label, so the
// UI does not have to duplicate the translations the backend already has.
type optionDescriptor struct {
	Value       string `json:"value"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	// NeedsHypervisor помечает режим проверки, которому нужен KVM-хост:
	// интерфейс по этому признаку спрашивает, где запускать, вместо того
	// чтобы дублировать список режимов у себя.
	NeedsHypervisor bool `json:"needs_hypervisor,omitempty"`
}

// metaResponse tells the SPA what this deployment can do, which is what lets
// the UI hide features that would only fail.
type metaResponse struct {
	BackupTypes  []optionDescriptor `json:"backup_types"`
	VerifyModes  []optionDescriptor `json:"verify_modes"`
	StorageKinds []optionDescriptor `json:"storage_kinds"`
	Actions      []optionDescriptor `json:"remediation_actions"`
	Roles        []optionDescriptor `json:"roles"`

	Capabilities struct {
		QemuImg           bool   `json:"qemu_img"`
		Encryption        bool   `json:"encryption"`
		Compression       string `json:"compression"`
		ChunkSize         int    `json:"chunk_size"`
		DatabaseType      string `json:"database_type"`
		SchedulerTZ       string `json:"scheduler_timezone"`
		RemediationOn     bool   `json:"remediation_enabled"`
		RemediationDryRun bool   `json:"remediation_dry_run"`
		AuthEnabled       bool   `json:"auth_enabled"`
	} `json:"capabilities"`

	DefaultRetention model.RetentionPolicy `json:"default_retention"`
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	resp := metaResponse{DefaultRetention: model.DefaultRetention()}

	descriptions := map[model.BackupType]string{
		model.BackupFull:         "Полная копия через Backup API. Создаёт checkpoint — основу для инкрементов. ВМ не останавливается.",
		model.BackupIncremental:  "Только блоки, изменившиеся с прошлого запуска. Самый быстрый и компактный вариант.",
		model.BackupDifferential: "Всё, что изменилось с последнего полного. Восстановление всегда из двух точек.",
		model.BackupSnapshot:     "Полная копия через временный снапшот. Работает без отслеживания изменённых блоков.",
		model.BackupOVA:          "Экспорт ВМ в OVA средствами движка. Файл остаётся на хосте гипервизора.",
		model.BackupConfig:       "Только описание ВМ. Секунды на выполнение, данные дисков не сохраняются.",
	}
	for _, t := range model.AllBackupTypes() {
		resp.BackupTypes = append(resp.BackupTypes, optionDescriptor{
			Value: string(t), Title: t.Title(), Description: descriptions[t],
		})
	}

	verifyDescriptions := map[model.VerifyMode]string{
		model.VerifyQuick:     "Проверяет, что все объекты на месте и нужного размера. Секунды.",
		model.VerifyChain:     "Проверяет, что цепочка собирается: все звенья есть, checkpoint-ы стыкуются.",
		model.VerifyManifest:  "Пересчитывает SHA-256 каждого чанка в хранилище. Читает весь бэкап.",
		model.VerifyStructure: "Читает таблицу разделов и суперблоки ФС внутри образа. Единственная проверка, которая ловит «бэкап цел, но внутри пусто».",
		model.VerifyRestore:   "Собирает образ во временный файл тем же кодом, что и настоящее восстановление.",
		model.VerifyQemu:      "Собирает qcow2 и прогоняет qemu-img check.",
		model.VerifySource:    "Сверяет сохранённое с тем, что отдаёт гипервизор. Для KVM выполняется в момент бэкапа, пока точка ещё открыта.",
		model.VerifyBoot:      "Запускает восстановленный образ в изолированной ВМ без сети и ждёт отклика гостевого агента. Самое сильное доказательство.",
	}
	for _, m := range model.AllVerifyModes() {
		resp.VerifyModes = append(resp.VerifyModes, optionDescriptor{
			Value: string(m), Title: m.Title(), Description: verifyDescriptions[m],
			NeedsHypervisor: m.NeedsHypervisor(),
		})
	}

	resp.StorageKinds = []optionDescriptor{
		{Value: string(model.StorageLocal), Title: "Локальный каталог", Description: "Локальный диск или примонтированная NFS/CIFS-шара."},
		{Value: string(model.StorageS3), Title: "S3-совместимое", Description: "AWS S3, MinIO, Ceph RGW и совместимые."},
		{Value: string(model.StorageSFTP), Title: "SFTP", Description: "Удалённый сервер по SSH."},
	}

	for _, a := range []model.RemediationAction{model.ActionVMStart, model.ActionVMUnpause,
		model.ActionVMReset, model.ActionHostActivate, model.ActionHostFence, model.ActionReconnect} {
		desc := ""
		if a.Disruptive() {
			desc = "Прерывает работу; требует явного подтверждения."
		}
		resp.Actions = append(resp.Actions, optionDescriptor{
			Value: string(a), Title: a.Title(), Description: desc,
		})
	}

	resp.Roles = []optionDescriptor{
		{Value: string(model.RoleAdmin), Title: "Администратор", Description: "Полный доступ, включая подключения, хранилища и пользователей."},
		{Value: string(model.RoleOperator), Title: "Оператор", Description: "Управление ВМ и бэкапами без правки конфигурации."},
		{Value: string(model.RoleViewer), Title: "Наблюдатель", Description: "Только чтение."},
	}

	resp.Capabilities.QemuImg = backup.QemuImgAvailable(s.cfg.Backup.QemuImgPath)
	resp.Capabilities.Encryption = true
	resp.Capabilities.Compression = s.cfg.Backup.Compression
	resp.Capabilities.ChunkSize = s.cfg.Backup.ChunkSize
	resp.Capabilities.DatabaseType = s.cfg.Database.Driver
	resp.Capabilities.SchedulerTZ = s.cfg.Scheduler.Timezone
	resp.Capabilities.RemediationOn = s.cfg.Monitor.Remediation.Enabled
	// Живое значение, а не настройка: режим переключается на ходу, и
	// интерфейс должен показывать то, что действует сейчас.
	resp.Capabilities.RemediationDryRun = s.remediator.DryRun()
	resp.Capabilities.AuthEnabled = s.cfg.Auth.Enabled

	writeJSON(w, http.StatusOK, resp)
}

// dashboardResponse is everything the landing page needs in one call, so the
// first paint does not depend on a dozen round trips.
type dashboardResponse struct {
	Servers    []*model.ServerSummary `json:"servers"`
	Alerts     []*model.Alert         `json:"alerts"`
	RecentRuns []*model.BackupRun     `json:"recent_runs"`
	Storages   []*model.StorageTarget `json:"storages"`
	Totals     struct {
		Servers        int   `json:"servers"`
		ServersOnline  int   `json:"servers_online"`
		Hosts          int   `json:"hosts"`
		HostsUp        int   `json:"hosts_up"`
		VMs            int   `json:"vms"`
		VMsUp          int   `json:"vms_up"`
		VMsPaused      int   `json:"vms_paused"`
		ProtectedVMs   int   `json:"protected_vms"`
		AlertsFiring   int   `json:"alerts_firing"`
		AlertsCritical int   `json:"alerts_critical"`
		RunningBackups int   `json:"running_backups"`
		StoredBytes    int64 `json:"stored_bytes"`
	} `json:"totals"`
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	servers, err := s.store.ListServers(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	resp := dashboardResponse{}
	for _, srv := range servers {
		summary, err := s.buildSummary(r.Context(), srv.ID)
		if err != nil {
			s.log.Warn().Err(err).Str("сервер", srv.Name).Msg("не удалось собрать сводку")
			continue
		}
		resp.Servers = append(resp.Servers, summary)

		resp.Totals.Servers++
		if srv.State == model.ConnOnline {
			resp.Totals.ServersOnline++
		}
		resp.Totals.Hosts += summary.HostsTotal
		resp.Totals.HostsUp += summary.HostsUp
		resp.Totals.VMs += summary.VMsTotal
		resp.Totals.VMsUp += summary.VMsUp
		resp.Totals.VMsPaused += summary.VMsPaused
		resp.Totals.ProtectedVMs += summary.ProtectedVMs
		resp.Totals.AlertsFiring += summary.AlertsFiring
		resp.Totals.AlertsCritical += summary.AlertsCritical
	}
	resp.Totals.RunningBackups = s.scheduler.RunningCount()

	alerts, err := s.store.ListAlerts(r.Context(), store.AlertFilter{
		States: []model.AlertState{model.AlertFiring},
		Limit:  25,
	})
	if err == nil {
		resp.Alerts = alerts
	}

	since := time.Now().Add(-7 * 24 * time.Hour)
	runs, err := s.store.ListBackupRuns(r.Context(), store.RunFilter{Since: &since, Limit: 25})
	if err == nil {
		resp.RecentRuns = runs
		for _, run := range runs {
			resp.Totals.StoredBytes += run.StoredBytes
		}
	}

	storages, err := s.store.ListStorageTargets(r.Context())
	if err == nil {
		resp.Storages = storages
	}

	if resp.Servers == nil {
		resp.Servers = []*model.ServerSummary{}
	}
	if resp.Alerts == nil {
		resp.Alerts = []*model.Alert{}
	}
	if resp.RecentRuns == nil {
		resp.RecentRuns = []*model.BackupRun{}
	}
	if resp.Storages == nil {
		resp.Storages = []*model.StorageTarget{}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	entries, err := s.store.ListAudit(r.Context(), queryInt(r, "limit", 200))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, entries)
}

// userPayload is the write shape of a local account.
type userPayload struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
	Disabled bool   `json:"disabled"`
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, users)
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var payload userPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	if payload.Username == "" || payload.Password == "" {
		s.writeError(w, r, badRequest("нужны имя пользователя и пароль"))
		return
	}
	if len(payload.Password) < 10 {
		s.writeError(w, r, badRequest("пароль должен быть не короче 10 символов"))
		return
	}
	role := model.Role(payload.Role)
	switch role {
	case model.RoleAdmin, model.RoleOperator, model.RoleViewer:
	default:
		s.writeError(w, r, badRequest("неизвестная роль: %q", payload.Role))
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(payload.Password), bcrypt.DefaultCost)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	user := &model.User{
		Username: payload.Username, PasswordHash: string(hash),
		Role: role, Disabled: payload.Disabled,
	}
	if err := s.store.CreateUser(r.Context(), user); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "user.create", model.ScopeServer, user.ID, true, user.Username)
	writeJSON(w, http.StatusCreated, user)
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	user, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	var payload userPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	if payload.Role != "" {
		user.Role = model.Role(payload.Role)
	}
	user.Disabled = payload.Disabled

	// An empty password means "leave it alone", matching how the servers and
	// storages endpoints treat their secrets.
	user.PasswordHash = ""
	if payload.Password != "" {
		if len(payload.Password) < 10 {
			s.writeError(w, r, badRequest("пароль должен быть не короче 10 символов"))
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(payload.Password), bcrypt.DefaultCost)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		user.PasswordHash = string(hash)
	}

	if err := s.store.UpdateUser(r.Context(), user); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "user.update", model.ScopeServer, id, true, user.Username)
	writeJSON(w, http.StatusOK, user)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	user, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if p := principalFrom(r.Context()); p != nil && p.Username == user.Username {
		s.writeError(w, r, badRequest("нельзя удалить учётную запись, под которой вы вошли"))
		return
	}

	// Removing the last administrator would lock everyone out of the service.
	if user.Role == model.RoleAdmin {
		users, err := s.store.ListUsers(r.Context())
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		admins := 0
		for _, u := range users {
			if u.Role == model.RoleAdmin && !u.Disabled {
				admins++
			}
		}
		if admins <= 1 {
			s.writeError(w, r, badRequest("нельзя удалить последнего администратора"))
			return
		}
	}

	if err := s.store.DeleteUser(r.Context(), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "user.delete", model.ScopeServer, id, true, user.Username)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
