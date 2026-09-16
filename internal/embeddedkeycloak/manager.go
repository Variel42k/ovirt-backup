// Package embeddedkeycloak owns the host-side lifecycle of the bundled
// Keycloak. It is used only by the root helper and never by the application
// process directly.
package embeddedkeycloak

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/hosthelper"
	"github.com/Variel42k/ovirt-backup/internal/keycloakadmin"
)

const (
	stateFileName      = "keycloak.json"
	helperClientID     = "jhvirt-host-helper"
	defaultHelperImage = "ovirt-backup-postgres:17-alpine-hardened"
	maxAdminResponse   = 4 << 20
)

var simpleNameRE = regexp.MustCompile(`^[A-Za-z0-9._@-]+$`)

type Config struct {
	ComposeDir    string
	StateDir      string
	TruststoreDir string
	VaultDir      string
	DockerBinary  string
}

type Manager struct {
	cfg           Config
	composeBinary string
	composePrefix []string
	mu            sync.Mutex
}

type state struct {
	PublicURL         string            `json:"public_url"`
	Realm             string            `json:"realm"`
	Port              int               `json:"port"`
	DirectTLS         bool              `json:"direct_tls"`
	AdminClientID     string            `json:"admin_client_id"`
	AdminClientSecret string            `json:"admin_client_secret"`
	RealmScoped       bool              `json:"realm_scoped"`
	AppSecrets        map[string]string `json:"app_secrets"`
	ConsoleAdminID    string            `json:"console_admin_id,omitempty"`
	ConsoleAdminName  string            `json:"console_admin_name,omitempty"`
}

func New(cfg Config) (*Manager, error) {
	if cfg.DockerBinary == "" {
		cfg.DockerBinary = "docker"
	}
	for label, path := range map[string]string{
		"compose": cfg.ComposeDir, "state": cfg.StateDir,
		"truststore": cfg.TruststoreDir, "vault": cfg.VaultDir,
	} {
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("каталог %s должен быть абсолютным", label)
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("каталог %s отсутствует, не является каталогом или является symlink", label)
		}
	}
	composePath := filepath.Join(cfg.ComposeDir, "docker-compose.yml")
	info, err := os.Lstat(composePath)
	if err != nil {
		return nil, fmt.Errorf("compose-файл встроенного Keycloak: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("compose-файл встроенного Keycloak имеет недопустимый тип")
	}
	envPath := filepath.Join(cfg.ComposeDir, ".env")
	info, err = os.Lstat(envPath)
	if err != nil {
		return nil, fmt.Errorf("compose .env встроенного Keycloak: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("compose .env встроенного Keycloak имеет недопустимый тип")
	}
	composeBinary := cfg.DockerBinary
	composePrefix := []string{"compose"}
	probeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, probeErr := command(probeCtx, cfg.ComposeDir, "", cfg.DockerBinary, "compose", "version")
	cancel()
	if probeErr != nil {
		legacy, lookupErr := exec.LookPath("docker-compose")
		if lookupErr != nil {
			return nil, errors.New("не найдены Docker Compose v2 и docker-compose v1")
		}
		composeBinary, composePrefix = legacy, nil
	}
	return &Manager{cfg: cfg, composeBinary: composeBinary, composePrefix: composePrefix}, nil
}

func (m *Manager) Status(ctx context.Context) (hosthelper.Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := hosthelper.Status{Available: true}
	st, err := m.loadState()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return out, err
	}
	if err == nil {
		out.Initialized, out.PublicURL, out.Realm, out.Port = true, st.PublicURL, st.Realm, st.Port
		out.DirectTLS = st.DirectTLS
	} else if env, envErr := readEnv(filepath.Join(m.cfg.ComposeDir, ".env")); envErr == nil {
		out.PublicURL = strings.TrimRight(strings.TrimSpace(env["JHV_KEYCLOAK_URL"]), "/")
		out.Port, _ = strconv.Atoi(env["KEYCLOAK_PORT"])
		out.DirectTLS = env["KEYCLOAK_DIRECT_TLS"] == "1"
		out.Realm = strings.TrimSpace(env["KEYCLOAK_REALM"])
		issuer := strings.TrimRight(strings.TrimSpace(env["JHV_OIDC_ISSUER"]), "/")
		if marker := strings.LastIndex(issuer, "/realms/"); out.Realm == "" && marker >= 0 {
			out.Realm = issuer[marker+len("/realms/"):]
		}
	}
	running, runErr := m.composeOutput(ctx, "ps", "--status", "running", "-q", "keycloak")
	if runErr == nil {
		out.Running = strings.TrimSpace(running) != ""
	}
	return out, nil
}

func (m *Manager) Bootstrap(ctx context.Context, request hosthelper.BootstrapRequest) (hosthelper.BootstrapResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out hosthelper.BootstrapResponse
	request.PublicURL = strings.TrimRight(strings.TrimSpace(request.PublicURL), "/")
	request.Realm = strings.TrimSpace(request.Realm)
	request.ClientID = strings.TrimSpace(request.ClientID)
	request.RedirectURL = strings.TrimSpace(request.RedirectURL)
	if err := validateBootstrap(request); err != nil {
		return out, err
	}
	envPath := filepath.Join(m.cfg.ComposeDir, ".env")
	env, err := readEnv(envPath)
	if err != nil {
		return out, fmt.Errorf("чтение compose .env: %w", err)
	}
	project := env["COMPOSE_PROJECT_NAME"]
	if project == "" {
		project = "ovirt-backup"
	}
	if !simpleNameRE.MatchString(project) {
		return out, errors.New("COMPOSE_PROJECT_NAME содержит недопустимые символы")
	}
	postgresUser := env["POSTGRES_USER"]
	if postgresUser == "" {
		postgresUser = "jhvirt"
	}
	if !simpleNameRE.MatchString(postgresUser) {
		return out, errors.New("POSTGRES_USER содержит недопустимые символы")
	}

	st, stateErr := m.loadState()
	fresh := errors.Is(stateErr, os.ErrNotExist)
	if stateErr != nil && !fresh {
		return out, stateErr
	}
	if !fresh && (st.PublicURL != request.PublicURL || st.Realm != request.Realm) {
		return out, fmt.Errorf("встроенный Keycloak уже инициализирован как %s/realms/%s; смена адреса или realm выполняется установщиком", st.PublicURL, st.Realm)
	}
	if fresh {
		st = state{
			PublicURL: request.PublicURL, Realm: request.Realm, Port: request.Port,
			DirectTLS: request.DirectTLS, AdminClientID: helperClientID,
			AppSecrets: map[string]string{},
		}
	}
	if st.AppSecrets == nil {
		st.AppSecrets = map[string]string{}
	}
	st.Port, st.DirectTLS = request.Port, request.DirectTLS

	keycloakVolume := project + "_keycloak-data"
	appVolume, err := m.applicationDataVolume(ctx, project)
	if err != nil {
		return out, err
	}
	image := helperImage(env)
	if err := m.ensureVolume(ctx, keycloakVolume); err != nil {
		return out, err
	}
	dbPassword, err := m.volumeRead(ctx, image, keycloakVolume, "ovirt-backup/database.password")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return out, err
	}
	if dbPassword == "" {
		dbPassword, err = randomSecret(24)
		if err != nil {
			return out, err
		}
		if err := m.volumeWrite(ctx, image, keycloakVolume, "ovirt-backup/database.password", dbPassword+"\n", "0400"); err != nil {
			return out, err
		}
	}
	bootstrapUser, bootstrapPassword := "", ""
	if fresh {
		suffix, secretErr := randomSecret(6)
		if secretErr != nil {
			return out, secretErr
		}
		bootstrapUser = "kc-web-bootstrap-" + strings.ToLower(suffix[:8])
		bootstrapPassword, err = randomSecret(24)
		if err != nil {
			return out, err
		}
	}
	if request.DirectTLS {
		if err := m.copyTLS(ctx, image, appVolume, keycloakVolume, request.PublicURL); err != nil {
			return out, err
		}
	}
	configBody := keycloakConfig(request, dbPassword, bootstrapUser, bootstrapPassword)
	if err := m.volumeWrite(ctx, image, keycloakVolume, "ovirt-backup/keycloak.conf", configBody, "0400"); err != nil {
		return out, err
	}
	profiles := addProfile(env["COMPOSE_PROFILES"], "keycloak")
	updates := map[string]string{
		"COMPOSE_PROFILES": profiles, "KEYCLOAK_PORT": strconv.Itoa(request.Port),
		"KEYCLOAK_BIND_ADDRESS": map[bool]string{true: "0.0.0.0", false: "127.0.0.1"}[request.DirectTLS], "KEYCLOAK_DB": "keycloak",
		"KEYCLOAK_DB_USER": "keycloak_app", "KEYCLOAK_REALM": request.Realm, "JHV_KEYCLOAK_URL": request.PublicURL,
		"KEYCLOAK_DIRECT_TLS":         boolNumber(request.DirectTLS),
		"KEYCLOAK_CONTAINER_PORT":     map[bool]string{true: "8443", false: "8080"}[request.DirectTLS],
		"JHV_KEYCLOAK_TRUSTSTORE_DIR": m.cfg.TruststoreDir,
		"JHV_KEYCLOAK_VAULT_DIR":      m.cfg.VaultDir,
	}
	if err := updateEnvFile(envPath, updates); err != nil {
		return out, err
	}
	if err := m.ensureDatabase(ctx, postgresUser, dbPassword); err != nil {
		return out, err
	}
	if err := os.MkdirAll(m.cfg.TruststoreDir, 0o750); err != nil {
		return out, err
	}
	if err := os.MkdirAll(m.cfg.VaultDir, 0o750); err != nil {
		return out, err
	}
	if _, err := m.composeOutput(ctx, "up", "-d", "--build", "--no-deps", "--force-recreate", "keycloak", "keycloak-backup"); err != nil {
		return out, fmt.Errorf("запуск контейнеров Keycloak: %w", err)
	}
	// Repair permissions of an already persisted LDAP bind secret as part of
	// every Keycloak bootstrap/update. Older releases could leave the host file
	// owned by the application UID/GID, while the Keycloak image runs with a
	// different UID and therefore could not read the file-vault credential.
	if err := m.ensureVaultSecretReadable(ctx, st.Realm, "ad-bind"); err != nil {
		return out, err
	}
	localURL := localURL(request.Port, request.DirectTLS)
	adminTLS, err := m.adminTLSConfig(ctx, image, keycloakVolume, request.PublicURL, request.DirectTLS)
	if err != nil {
		return out, err
	}
	if err := m.waitReady(ctx, localURL, adminTLS); err != nil {
		return out, err
	}
	admin := newAdminAPI(localURL, adminTLS)
	recoveryClientID := ""
	if fresh {
		if authErr := admin.authenticatePassword(ctx, "master", bootstrapUser, bootstrapPassword); authErr != nil {
			var recoveryErr error
			recoveryClientID, recoveryErr = m.startRecoveryClient(ctx, admin, localURL, adminTLS)
			if recoveryErr != nil {
				return out, fmt.Errorf("bootstrap и восстановительная авторизация Keycloak недоступны: %w", recoveryErr)
			}
		}
		st.AdminClientSecret, err = randomSecret(32)
		if err != nil {
			return out, err
		}
	} else if err := admin.authenticateClient(ctx, "master", st.AdminClientID, st.AdminClientSecret); err != nil {
		return out, fmt.Errorf("служебная учётная запись встроенного Keycloak недоступна: %w", err)
	}
	if fresh {
		if err := admin.ensureRealm(ctx, request.Realm); err != nil {
			return out, err
		}
		if err := admin.ensureServiceClient(ctx, st.AdminClientID, st.AdminClientSecret); err != nil {
			return out, err
		}
		if err := admin.ensureRealmAdminAccess(ctx, request.Realm, st.AdminClientID); err != nil {
			return out, err
		}
		helperAdmin, helperErr := authenticatedRealmAdmin(ctx, localURL, adminTLS, request.Realm,
			st.AdminClientID, st.AdminClientSecret)
		if helperErr != nil {
			return out, helperErr
		}
		if err := helperAdmin.verifyMasterAdminRevoked(ctx, request.Realm); err != nil {
			return out, err
		}
		currentBootstrapUser := bootstrapUser
		if recoveryClientID != "" {
			currentBootstrapUser = ""
		}
		if err := admin.cleanupTemporaryPrincipals(ctx, currentBootstrapUser, recoveryClientID); err != nil {
			return out, err
		}
		configBody = keycloakConfig(request, dbPassword, "", "")
		if err := m.volumeWrite(ctx, image, keycloakVolume, "ovirt-backup/keycloak.conf", configBody, "0400"); err != nil {
			return out, err
		}
		st.RealmScoped = true
		if err := m.saveState(st); err != nil {
			return out, err
		}
		admin = helperAdmin
	} else if !st.RealmScoped {
		// Older web-bootstrap versions left the persistent helper as a global
		// master administrator. Use that one last time to repair the explicit
		// target-realm mapping, remove stale bootstrap identities, and revoke
		// the global role.
		if err := admin.ensureRealm(ctx, request.Realm); err != nil {
			return out, err
		}
		if err := admin.ensureServiceClient(ctx, st.AdminClientID, st.AdminClientSecret); err != nil {
			return out, err
		}
		if err := admin.ensureRealmAdminAccess(ctx, request.Realm, st.AdminClientID); err != nil {
			return out, err
		}
		helperAdmin, helperErr := authenticatedRealmAdmin(ctx, localURL, adminTLS, request.Realm,
			st.AdminClientID, st.AdminClientSecret)
		if helperErr != nil {
			return out, helperErr
		}
		if err := admin.cleanupTemporaryPrincipals(ctx, "", ""); err != nil {
			return out, err
		}
		if err := admin.removeServiceClientMasterAdmin(ctx, st.AdminClientID); err != nil {
			return out, err
		}
		if err := helperAdmin.authenticateClient(ctx, "master", st.AdminClientID, st.AdminClientSecret); err != nil {
			return out, fmt.Errorf("проверка helper после отзыва глобальной роли: %w", err)
		}
		if err := helperAdmin.verifyRealmAccess(ctx, request.Realm); err != nil {
			return out, fmt.Errorf("проверка realm-доступа helper после отзыва глобальной роли: %w", err)
		}
		if err := helperAdmin.verifyMasterAdminRevoked(ctx, request.Realm); err != nil {
			return out, err
		}
		st.RealmScoped = true
		if err := m.saveState(st); err != nil {
			return out, err
		}
		admin = helperAdmin
	} else if err := admin.verifyRealmAccess(ctx, request.Realm); err != nil {
		return out, fmt.Errorf("служебная учётная запись не имеет доступа к realm %s: %w", request.Realm, err)
	} else if err := admin.verifyMasterAdminRevoked(ctx, request.Realm); err != nil {
		return out, err
	}
	for group := range request.RoleMapping {
		if err := admin.ensureGroup(ctx, request.Realm, group); err != nil {
			return out, err
		}
	}
	appKey := request.Realm + "/" + request.ClientID
	appSecret := st.AppSecrets[appKey]
	if appSecret == "" {
		appSecret, err = randomSecret(32)
		if err != nil {
			return out, err
		}
		st.AppSecrets[appKey] = appSecret
		if err := m.saveState(st); err != nil {
			return out, err
		}
	}
	kc, err := keycloakadmin.NewWithBackchannelTransport(request.PublicURL+"/realms/"+request.Realm,
		localURL, "master", st.AdminClientID, st.AdminClientSecret, tlsTransport(adminTLS))
	if err != nil {
		return out, err
	}
	if err := kc.EnsureApplicationClient(ctx, request.ClientID, appSecret, request.RedirectURL); err != nil {
		return out, err
	}
	if err := admin.secureRealm(ctx, request.Realm); err != nil {
		return out, err
	}
	if err := m.saveState(st); err != nil {
		return out, err
	}
	status := hosthelper.Status{Available: true, Initialized: true, Running: true,
		PublicURL: st.PublicURL, Realm: st.Realm, Port: st.Port, DirectTLS: st.DirectTLS}
	return hosthelper.BootstrapResponse{
		Status: status, Issuer: request.PublicURL + "/realms/" + request.Realm,
		BackchannelURL: "http://keycloak:8080", ClientSecret: appSecret,
	}, nil
}

func (m *Manager) ConfigureDomain(ctx context.Context, request hosthelper.DomainRequest) (hosthelper.DomainResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out hosthelper.DomainResponse
	st, err := m.loadState()
	if err != nil {
		return out, errors.New("встроенный Keycloak ещё не инициализирован")
	}
	wantedIssuer := st.PublicURL + "/realms/" + st.Realm
	if strings.TrimRight(strings.TrimSpace(request.Issuer), "/") != wantedIssuer {
		return out, errors.New("issuer не принадлежит встроенному Keycloak этой установки")
	}
	env, err := readEnv(filepath.Join(m.cfg.ComposeDir, ".env"))
	if err != nil {
		return out, fmt.Errorf("чтение compose .env: %w", err)
	}
	project := env["COMPOSE_PROJECT_NAME"]
	if project == "" {
		project = "ovirt-backup"
	}
	if !simpleNameRE.MatchString(project) {
		return out, errors.New("COMPOSE_PROJECT_NAME содержит недопустимые символы")
	}
	adminTLS, err := m.adminTLSConfig(ctx, helperImage(env), project+"_keycloak-data", st.PublicURL, st.DirectTLS)
	if err != nil {
		return out, err
	}
	rollbackCA := func() {}
	caChanged := false
	if request.CACertificate != "" {
		if err := validateCACertificates(request.CACertificate); err != nil {
			return out, err
		}
		caPath := filepath.Join(m.cfg.TruststoreDir, st.Realm+"-ad-ca.pem")
		previousCA, previousCAErr := os.ReadFile(caPath)
		if previousCAErr != nil && !errors.Is(previousCAErr, os.ErrNotExist) {
			return out, previousCAErr
		}
		changed, err := writeIfChanged(caPath, []byte(request.CACertificate), 0o444)
		if err != nil {
			return out, err
		}
		if changed {
			caChanged = true
			rollbackCA = func() {
				if previousCAErr == nil {
					_ = atomicWrite(caPath, previousCA, 0o444)
				} else {
					_ = os.Remove(caPath)
				}
			}
			if _, err := m.composeOutput(ctx, "restart", "keycloak"); err != nil {
				rollbackCA()
				restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 2*time.Minute)
				_, _ = m.composeOutput(restoreCtx, "restart", "keycloak")
				restoreCancel()
				return out, fmt.Errorf("перезапуск Keycloak после добавления CA: %w", err)
			}
			if err := m.waitReady(ctx, localURL(st.Port, st.DirectTLS), adminTLS); err != nil {
				rollbackCA()
				restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 2*time.Minute)
				_, _ = m.composeOutput(restoreCtx, "restart", "keycloak")
				restoreCancel()
				return out, err
			}
		}
	}
	vaultName := vaultFileName(st.Realm, "ad-bind")
	vaultPath := filepath.Join(m.cfg.VaultDir, vaultName)
	previous, previousErr := os.ReadFile(vaultPath)
	if previousErr != nil && !errors.Is(previousErr, os.ErrNotExist) {
		return out, previousErr
	}
	if err := writeSecret(vaultPath, request.Domain.BindPassword); err != nil {
		return out, err
	}
	if err := m.ensureVaultSecretReadable(ctx, st.Realm, "ad-bind"); err != nil {
		if previousErr == nil {
			_ = writeSecret(vaultPath, string(previous))
		} else {
			_ = os.Remove(vaultPath)
		}
		return out, err
	}
	rollback := func() {
		if previousErr == nil {
			_ = writeSecret(vaultPath, string(previous))
			repairCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = m.ensureVaultSecretReadable(repairCtx, st.Realm, "ad-bind")
			cancel()
		} else {
			_ = os.Remove(vaultPath)
		}
		if caChanged {
			rollbackCA()
			rollbackCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			_, _ = m.composeOutput(rollbackCtx, "restart", "keycloak")
			cancel()
		}
	}
	kc, err := keycloakadmin.NewWithBackchannelTransport(wantedIssuer, localURL(st.Port, st.DirectTLS),
		"master", st.AdminClientID, st.AdminClientSecret, tlsTransport(adminTLS))
	if err != nil {
		rollback()
		return out, err
	}
	request.Domain.StoredBindCredential = "${vault.ad-bind}"
	result, err := kc.ConfigureDomain(ctx, request.Domain)
	request.Domain.BindPassword = ""
	if err != nil {
		rollback()
		return out, err
	}

	// The live LDAP test above uses the raw password, while the persisted
	// provider deliberately stores only a file-vault expression. A domain
	// setup is therefore not complete until that persisted credential is
	// proven to survive the same Keycloak restart that previously exposed
	// broken post-install/domain-join configurations.
	if _, err := m.composeOutput(ctx, "restart", "keycloak"); err != nil {
		return out, fmt.Errorf("перезапуск Keycloak для проверки сохранённого bind-секрета: %w", err)
	}
	if err := m.waitReady(ctx, localURL(st.Port, st.DirectTLS), adminTLS); err != nil {
		return out, fmt.Errorf("Keycloak не поднялся после сохранения домена: %w", err)
	}
	kc, err = keycloakadmin.NewWithBackchannelTransport(wantedIssuer, localURL(st.Port, st.DirectTLS),
		"master", st.AdminClientID, st.AdminClientSecret, tlsTransport(adminTLS))
	if err != nil {
		return out, err
	}
	persistedStatus, err := kc.VerifyStoredDomain(ctx, request.Domain.ProviderName, request.Domain.StoredBindCredential)
	if err != nil {
		return out, fmt.Errorf("сохранённая настройка Active Directory не прошла проверку после перезапуска Keycloak: %w", err)
	}
	if strings.TrimSpace(persistedStatus) != "" {
		result.UsersStatus = strings.TrimSpace(result.UsersStatus + "; после перезапуска: " + persistedStatus)
	}
	out.Result = result
	return out, nil
}

func validateBootstrap(request hosthelper.BootstrapRequest) error {
	u, err := url.Parse(request.PublicURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") {
		return errors.New("публичный URL Keycloak должен быть origin без пути, параметров и учётных данных")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback(u.Hostname())) {
		return errors.New("публичный URL Keycloak должен использовать HTTPS")
	}
	if request.DirectTLS && u.Scheme != "https" {
		return errors.New("прямой TLS требует публичный HTTPS URL")
	}
	if request.Port < 1 || request.Port > 65535 {
		return errors.New("порт Keycloak должен быть от 1 до 65535")
	}
	if request.DirectTLS {
		publicPort := 443
		if u.Port() != "" {
			publicPort, _ = strconv.Atoi(u.Port())
		}
		if publicPort != request.Port {
			return errors.New("при прямом TLS порт публичного URL должен совпадать с портом Keycloak")
		}
	}
	for label, value := range map[string]string{"realm": request.Realm, "client ID": request.ClientID} {
		if !simpleNameRE.MatchString(value) || value == "." || value == ".." || len(value) > 255 {
			return fmt.Errorf("%s задан неверно", label)
		}
	}
	redirect, err := url.Parse(request.RedirectURL)
	if err != nil || redirect.Scheme != "https" || redirect.Host == "" ||
		!strings.HasSuffix(redirect.Path, "/api/v1/auth/oidc/callback") {
		return errors.New("redirect URL должен быть HTTPS-адресом callback приложения")
	}
	for group, role := range request.RoleMapping {
		if !simpleNameRE.MatchString(group) || len(group) > 255 || (role != "admin" && role != "operator" && role != "viewer") {
			return errors.New("имя группы доступа задано неверно")
		}
	}
	return nil
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func keycloakConfig(request hosthelper.BootstrapRequest, dbPassword, bootstrapUser, bootstrapPassword string) string {
	var b strings.Builder
	fmt.Fprintln(&b, "db=postgres")
	fmt.Fprintln(&b, "db-url=jdbc:postgresql://postgres:5432/keycloak")
	fmt.Fprintln(&b, "db-username=keycloak_app")
	fmt.Fprintf(&b, "db-password=%s\n", dbPassword)
	fmt.Fprintf(&b, "hostname=%s\n", request.PublicURL)
	fmt.Fprintln(&b, "hostname-strict=true")
	fmt.Fprintln(&b, "http-enabled=true")
	fmt.Fprintln(&b, "http-port=8080")
	fmt.Fprintln(&b, "health-enabled=true")
	fmt.Fprintln(&b, "cache=local")
	if request.DirectTLS {
		fmt.Fprintln(&b, "https-port=8443")
		fmt.Fprintln(&b, "https-certificate-file=/opt/keycloak/data/ovirt-backup/server.crt")
		fmt.Fprintln(&b, "https-certificate-key-file=/opt/keycloak/data/ovirt-backup/server.key")
	} else if strings.HasPrefix(request.PublicURL, "https://") {
		fmt.Fprintln(&b, "proxy-headers=xforwarded")
	}
	if bootstrapUser != "" {
		fmt.Fprintf(&b, "bootstrap-admin-username=%s\n", bootstrapUser)
		fmt.Fprintf(&b, "bootstrap-admin-password=%s\n", bootstrapPassword)
	}
	return b.String()
}

func (m *Manager) ensureDatabase(ctx context.Context, postgresUser, password string) error {
	sql := fmt.Sprintf(`SELECT 'CREATE ROLE "keycloak_app" LOGIN' WHERE NOT EXISTS
  (SELECT 1 FROM pg_roles WHERE rolname='keycloak_app') \gexec
ALTER ROLE "keycloak_app" WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD '%s';
SELECT 'CREATE DATABASE keycloak OWNER "keycloak_app"' WHERE NOT EXISTS
  (SELECT 1 FROM pg_database WHERE datname='keycloak') \gexec
ALTER DATABASE keycloak OWNER TO "keycloak_app";
REVOKE CONNECT ON DATABASE keycloak FROM PUBLIC;
GRANT CONNECT ON DATABASE keycloak TO "keycloak_app", "%s";
`, password, postgresUser)
	_, err := m.composeInput(ctx, sql, "exec", "-T", "postgres", "psql", "-U", postgresUser,
		"-d", "postgres", "-v", "ON_ERROR_STOP=1")
	if err != nil {
		return fmt.Errorf("подготовка базы Keycloak: %w", err)
	}
	return nil
}

func (m *Manager) applicationDataVolume(ctx context.Context, project string) (string, error) {
	cid, err := m.composeOutput(ctx, "ps", "-q", "ovirt-backup")
	if err == nil && strings.TrimSpace(cid) != "" {
		out, inspectErr := m.dockerOutput(ctx, "inspect", "--format",
			`{{range .Mounts}}{{if eq .Destination "/app/data"}}{{.Name}}{{end}}{{end}}`, strings.TrimSpace(cid))
		if inspectErr == nil && strings.TrimSpace(out) != "" {
			return strings.TrimSpace(out), nil
		}
	}
	return project + "_jhvirt-data", nil
}

func helperImage(env map[string]string) string {
	version := strings.TrimSpace(env["POSTGRES_VERSION"])
	if version == "" {
		return defaultHelperImage
	}
	if !simpleNameRE.MatchString(version) {
		return defaultHelperImage
	}
	return "ovirt-backup-postgres:" + version
}

func (m *Manager) ensureVolume(ctx context.Context, name string) error {
	_, err := m.dockerOutput(ctx, "volume", "create", "--label", "com.justhpc.virt-manager.volume=keycloak-data", name)
	return err
}

func (m *Manager) volumeRead(ctx context.Context, image, volume, path string) (string, error) {
	out, err := m.dockerOutput(ctx, "run", "--rm", "--network", "none", "--user", "root",
		"-v", volume+":/data:ro", image, "sh", "-c", "test -f /data/"+path+" || exit 44; cat /data/"+path)
	if err != nil {
		if exitCode(err) == 44 {
			return "", os.ErrNotExist
		}
		return "", fmt.Errorf("чтение тома Keycloak: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func (m *Manager) volumeWrite(ctx context.Context, image, volume, path, value, mode string) error {
	script := "set -eu; mkdir -p /data/ovirt-backup; tmp=/data/" + path + ".tmp.$$; cat > \"$tmp\"; chown 1000:0 \"$tmp\"; chmod " + mode + " \"$tmp\"; mv -f \"$tmp\" /data/" + path + "; chown 1000:0 /data /data/ovirt-backup; chmod 0750 /data /data/ovirt-backup"
	_, err := m.dockerInput(ctx, value, volumeWriteRunArgs(image, volume, script)...)
	if err != nil {
		return fmt.Errorf("запись тома Keycloak: %w", err)
	}
	written, err := m.volumeRead(ctx, image, volume, path)
	if err != nil {
		return fmt.Errorf("проверка записи тома Keycloak: %w", err)
	}
	if written != strings.TrimSpace(value) {
		return errors.New("проверка записи тома Keycloak: записанное значение не совпадает с исходным")
	}
	return nil
}

func volumeWriteRunArgs(image, volume, script string) []string {
	return []string{"run", "--rm", "-i", "--network", "none", "--user", "root",
		"-v", volume + ":/data", image, "sh", "-c", script}
}

func (m *Manager) copyTLS(ctx context.Context, image, sourceVolume, targetVolume, publicURL string) error {
	certPEM, certErr := m.volumeRead(ctx, image, sourceVolume, "tls/server.crt")
	keyPEM, keyErr := m.volumeRead(ctx, image, sourceVolume, "tls/server.key")
	if certErr != nil || keyErr != nil {
		return errors.New("для прямого TLS встроенного Keycloak в томе приложения нет сертификата и закрытого ключа")
	}
	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil || len(pair.Certificate) == 0 {
		return errors.New("TLS-сертификат приложения и закрытый ключ не образуют корректную пару")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return errors.New("не удалось прочитать TLS-сертификат приложения")
	}
	u, _ := url.Parse(publicURL)
	if err := leaf.VerifyHostname(u.Hostname()); err != nil {
		return fmt.Errorf("TLS-сертификат приложения не подходит имени Keycloak %s", u.Hostname())
	}
	script := `set -eu
test -s /source/tls/server.crt
test -s /source/tls/server.key
mkdir -p /target/ovirt-backup
cp /source/tls/server.crt /target/ovirt-backup/server.crt
cp /source/tls/server.key /target/ovirt-backup/server.key
chown 1000:0 /target/ovirt-backup/server.crt /target/ovirt-backup/server.key
chmod 0444 /target/ovirt-backup/server.crt
chmod 0400 /target/ovirt-backup/server.key`
	_, err = m.dockerOutput(ctx, "run", "--rm", "--network", "none", "--user", "root",
		"-v", sourceVolume+":/source:ro", "-v", targetVolume+":/target", image, "sh", "-c", script)
	if err != nil {
		return errors.New("не удалось установить TLS-сертификат приложения во встроенный Keycloak")
	}
	return nil
}

// startRecoveryClient adopts a Keycloak database created by an older
// installer. The one-time service client is created offline, used to mint the
// persistent helper client, and deleted before Bootstrap returns.
func (m *Manager) startRecoveryClient(ctx context.Context, admin *adminAPI, baseURL string, tlsConfig *tls.Config) (string, error) {
	suffix, err := randomSecret(6)
	if err != nil {
		return "", err
	}
	clientID := "kc-web-recovery-" + strings.ToLower(suffix[:8])
	secret, err := randomSecret(24)
	if err != nil {
		return "", err
	}
	if _, err := m.composeOutput(ctx, "stop", "keycloak"); err != nil {
		return "", fmt.Errorf("остановка Keycloak для безопасного восстановления: %w", err)
	}
	restart := func() {
		_, _ = m.composeOutput(ctx, "up", "-d", "--no-deps", "keycloak")
	}
	_, err = m.composeOutputEnv(ctx, map[string]string{"KC_RECOVERY_SECRET": secret},
		"run", "--rm", "--no-deps", "-e", "KC_RECOVERY_SECRET", "keycloak",
		"--config-file=/opt/keycloak/data/ovirt-backup/keycloak.conf",
		"bootstrap-admin", "service", "--optimized", "--client-id", clientID,
		"--client-secret:env=KC_RECOVERY_SECRET", "--no-prompt")
	if err != nil {
		restart()
		return "", fmt.Errorf("создание одноразового recovery-клиента: %w", err)
	}
	if _, err := m.composeOutput(ctx, "up", "-d", "--no-deps", "keycloak"); err != nil {
		return "", fmt.Errorf("повторный запуск Keycloak: %w", err)
	}
	if err := m.waitReady(ctx, baseURL, tlsConfig); err != nil {
		return "", err
	}
	if err := admin.authenticateClient(ctx, "master", clientID, secret); err != nil {
		return "", fmt.Errorf("авторизация recovery-клиента: %w", err)
	}
	return clientID, nil
}

func (m *Manager) composeOutput(ctx context.Context, args ...string) (string, error) {
	return command(ctx, m.cfg.ComposeDir, "", m.composeBinary, append(m.composeArgs(), args...)...)
}

func (m *Manager) composeInput(ctx context.Context, input string, args ...string) (string, error) {
	return command(ctx, m.cfg.ComposeDir, input, m.composeBinary, append(m.composeArgs(), args...)...)
}

func (m *Manager) composeOutputEnv(ctx context.Context, env map[string]string, args ...string) (string, error) {
	return commandEnv(ctx, m.cfg.ComposeDir, "", env, m.composeBinary, append(m.composeArgs(), args...)...)
}

func (m *Manager) composeArgs() []string { return append([]string(nil), m.composePrefix...) }

func (m *Manager) dockerOutput(ctx context.Context, args ...string) (string, error) {
	return command(ctx, "", "", m.cfg.DockerBinary, args...)
}

func (m *Manager) dockerInput(ctx context.Context, input string, args ...string) (string, error) {
	return command(ctx, "", input, m.cfg.DockerBinary, args...)
}

func command(ctx context.Context, dir, input, name string, args ...string) (string, error) {
	return commandEnv(ctx, dir, input, nil, name, args...)
}

func commandEnv(ctx context.Context, dir, input string, extraEnv map[string]string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if len(extraEnv) > 0 {
		cmd.Env = os.Environ()
		for key, value := range extraEnv {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if len(message) > 2000 {
			message = message[len(message)-2000:]
		}
		if message != "" {
			return "", fmt.Errorf("%w: %s", err, message)
		}
		return "", err
	}
	return stdout.String(), nil
}

func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func randomSecret(bytesCount int) (string, error) {
	raw := make([]byte, bytesCount)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("генерация секрета: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func localURL(port int, directTLS bool) string {
	if directTLS {
		return "https://127.0.0.1:" + strconv.Itoa(port)
	}
	return "http://127.0.0.1:" + strconv.Itoa(port)
}

func (m *Manager) adminTLSConfig(ctx context.Context, image, volume, publicURL string, directTLS bool) (*tls.Config, error) {
	if !directTLS {
		return nil, nil
	}
	certificate, err := m.volumeRead(ctx, image, volume, "ovirt-backup/server.crt")
	if err != nil {
		return nil, fmt.Errorf("чтение TLS-сертификата встроенного Keycloak: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(certificate)) {
		return nil, errors.New("TLS-сертификат встроенного Keycloak имеет неверный PEM-формат")
	}
	public, err := url.Parse(publicURL)
	if err != nil || public.Hostname() == "" {
		return nil, errors.New("не удалось определить TLS-имя встроенного Keycloak")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: public.Hostname()}, nil
}

func tlsTransport(config *tls.Config) http.RoundTripper {
	if config == nil {
		return nil
	}
	return &http.Transport{TLSClientConfig: config.Clone()}
}

func (m *Manager) waitReady(ctx context.Context, baseURL string, tlsConfig *tls.Config) error {
	transport := tlsTransport(tlsConfig)
	client := &http.Client{Timeout: 5 * time.Second, Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return errors.New("redirect") }}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/realms/master", nil)
		if err == nil {
			resp, requestErr := client.Do(req)
			if requestErr == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		if detail, failed := m.keycloakStartFailure(ctx); failed {
			return fmt.Errorf("Keycloak завершился при запуске: %s", detail)
		}
		select {
		case <-ctx.Done():
			detail, _ := m.keycloakStartFailure(context.Background())
			if detail != "" {
				return fmt.Errorf("Keycloak не стал готов до истечения тайм-аута: %s", detail)
			}
			return errors.New("Keycloak не стал готов до истечения тайм-аута")
		case <-ticker.C:
		}
	}
}

func (m *Manager) keycloakStartFailure(ctx context.Context) (string, bool) {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cid, err := m.composeOutput(checkCtx, "ps", "-a", "-q", "keycloak")
	if err != nil || strings.TrimSpace(cid) == "" {
		return "", false
	}
	state, err := m.dockerOutput(checkCtx, "inspect", "--format",
		`{{.State.Status}} {{.State.ExitCode}} {{.RestartCount}}`, strings.TrimSpace(cid))
	if err != nil {
		return "", false
	}
	fields := strings.Fields(state)
	if len(fields) != 3 {
		return "", false
	}
	restarts, _ := strconv.Atoi(fields[2])
	if !keycloakContainerFailed(fields[0]) {
		return "", false
	}
	logs, logErr := m.composeOutput(checkCtx, "logs", "--no-color", "--tail", "30", "keycloak")
	detail := fmt.Sprintf("status=%s, exit=%s, restarts=%d", fields[0], fields[1], restarts)
	if logErr == nil && strings.TrimSpace(logs) != "" {
		detail += "; " + sanitizeDiagnostic(logs)
	}
	return detail, true
}

func keycloakContainerFailed(status string) bool {
	return status == "exited" || status == "dead" || status == "restarting"
}

var diagnosticSecretRE = regexp.MustCompile(`(?i)(password|secret|credential)([[:space:]]*[=:][[:space:]]*)[^[:space:],;]+`)

func sanitizeDiagnostic(value string) string {
	value = strings.ReplaceAll(value, "\x1b", "")
	value = diagnosticSecretRE.ReplaceAllString(value, "$1$2[REDACTED]")
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 4000 {
		value = value[len(value)-4000:]
	}
	return value
}

func (m *Manager) statePath() string { return filepath.Join(m.cfg.StateDir, stateFileName) }

func (m *Manager) loadState() (state, error) {
	var out state
	raw, err := os.ReadFile(m.statePath())
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("состояние helper повреждено: %w", err)
	}
	if out.AdminClientID == "" || out.AdminClientSecret == "" || out.PublicURL == "" || out.Realm == "" {
		return out, errors.New("состояние helper неполно")
	}
	return out, nil
}

func (m *Manager) saveState(value state) error {
	if err := os.MkdirAll(m.cfg.StateDir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(m.statePath(), append(raw, '\n'), 0o600)
}

func readEnv(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok {
			out[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return out, nil
}

func updateEnvFile(path string, updates map[string]string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for key, value := range updates {
		if strings.ContainsAny(key+value, "\r\n") || strings.Contains(value, "$") || strings.Contains(value, "`") {
			return fmt.Errorf("недопустимое значение %s", key)
		}
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	seen := map[string]bool{}
	for i, line := range lines {
		key, _, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || strings.HasPrefix(key, "#") {
			continue
		}
		if value, exists := updates[key]; exists {
			lines[i], seen[key] = key+"="+value, true
		}
	}
	for key, value := range updates {
		if !seen[key] {
			lines = append(lines, key+"="+value)
		}
	}
	return atomicWrite(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

func atomicWrite(path string, body []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func addProfile(current, wanted string) string {
	seen := map[string]bool{}
	var profiles []string
	for _, item := range strings.Split(current, ",") {
		item = strings.TrimSpace(item)
		if item != "" && !seen[item] {
			profiles, seen[item] = append(profiles, item), true
		}
	}
	if !seen[wanted] {
		profiles = append(profiles, wanted)
	}
	return strings.Join(profiles, ",")
}

func boolNumber(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func validateCACertificates(value string) error {
	if len(value) > 1<<20 {
		return errors.New("цепочка CA слишком велика")
	}
	rest := []byte(value)
	count := 0
	for len(bytes.TrimSpace(rest)) > 0 {
		block, remaining := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" {
			return errors.New("CA должен содержать только PEM-сертификаты")
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return errors.New("CA содержит некорректный сертификат")
		}
		count++
		rest = remaining
	}
	if count == 0 {
		return errors.New("цепочка CA пуста")
	}
	return nil
}

func writeIfChanged(path string, body []byte, mode os.FileMode) (bool, error) {
	current, err := os.ReadFile(path)
	if err == nil && bytes.Equal(bytes.TrimSpace(current), bytes.TrimSpace(body)) {
		return false, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return true, atomicWrite(path, append(bytes.TrimSpace(body), '\n'), mode)
}

func writeSecret(path, value string) error {
	if value == "" || len(value) > 64*1024 || strings.ContainsAny(value, "\r\n") {
		return errors.New("bind-пароль пуст или имеет недопустимый формат")
	}
	return atomicWrite(path, []byte(value), 0o440)
}

// ensureVaultSecretReadable normalizes ownership/mode of a host-mounted
// file-vault secret and proves that the actual Keycloak container user can
// read it. The helper itself may create the file with its own host UID/GID,
// which is not necessarily the UID/GID used by the Keycloak image.
func (m *Manager) ensureVaultSecretReadable(ctx context.Context, realm, key string) error {
	name := vaultFileName(realm, key)
	hostPath := filepath.Join(m.cfg.VaultDir, name)
	info, err := os.Lstat(hostPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("проверка bind-секрета Keycloak: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("bind-секрет Keycloak имеет недопустимый тип")
	}

	gidOut, err := m.composeOutput(ctx, "exec", "-T", "keycloak", "id", "-g")
	if err != nil {
		return fmt.Errorf("определение группы процесса Keycloak: %w", err)
	}
	gid := strings.TrimSpace(gidOut)
	if _, err := strconv.ParseUint(gid, 10, 31); err != nil {
		return fmt.Errorf("Keycloak вернул некорректный gid %q", gid)
	}
	containerDir := "/opt/keycloak/conf/vault"
	containerPath := containerDir + "/" + name
	owner := "0:" + gid
	if _, err := m.composeOutput(ctx, "exec", "-T", "--user", "0:0", "keycloak", "chown", owner, containerDir, containerPath); err != nil {
		return fmt.Errorf("назначение владельца bind-секрета Keycloak: %w", err)
	}
	if _, err := m.composeOutput(ctx, "exec", "-T", "--user", "0:0", "keycloak", "chmod", "0750", containerDir); err != nil {
		return fmt.Errorf("права каталога vault Keycloak: %w", err)
	}
	if _, err := m.composeOutput(ctx, "exec", "-T", "--user", "0:0", "keycloak", "chmod", "0440", containerPath); err != nil {
		return fmt.Errorf("права bind-секрета Keycloak: %w", err)
	}
	if _, err := m.composeOutput(ctx, "exec", "-T", "keycloak", "/bin/sh", "-ec", `test -r "$1"`, "--", containerPath); err != nil {
		return errors.New("Keycloak не может прочитать bind-секрет после исправления прав; проверьте SELinux label каталога vault")
	}
	return nil
}

func vaultFileName(realm, key string) string {
	return strings.ReplaceAll(realm, "_", "__") + "_" + strings.ReplaceAll(key, "_", "__")
}

type adminAPI struct {
	base  string
	token string
	http  *http.Client
}

func newAdminAPI(base string, tlsConfig *tls.Config) *adminAPI {
	return &adminAPI{base: strings.TrimRight(base, "/"), http: &http.Client{
		Timeout:   2 * time.Minute,
		Transport: tlsTransport(tlsConfig),
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("Keycloak перенаправил административный запрос")
		},
	}}
}

func authenticatedRealmAdmin(ctx context.Context, base string, tlsConfig *tls.Config, realm, clientID, secret string) (*adminAPI, error) {
	admin := newAdminAPI(base, tlsConfig)
	if err := admin.authenticateClient(ctx, "master", clientID, secret); err != nil {
		return nil, fmt.Errorf("авторизация realm-scoped helper: %w", err)
	}
	if err := admin.verifyRealmAccess(ctx, realm); err != nil {
		return nil, fmt.Errorf("проверка realm-scoped helper: %w", err)
	}
	return admin, nil
}

func (a *adminAPI) authenticatePassword(ctx context.Context, realm, username, password string) error {
	form := url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {username}, "password": {password}}
	return a.authenticate(ctx, realm, form)
}

func (a *adminAPI) authenticateClient(ctx context.Context, realm, clientID, secret string) error {
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {secret}}
	return a.authenticate(ctx, realm, form)
}

func (a *adminAPI) authenticate(ctx context.Context, realm string, form url.Values) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/realms/"+url.PathEscape(realm)+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("авторизация helper в Keycloak: %w", err)
	}
	raw, err := readAdminResponse(resp)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Keycloak отклонил служебную авторизацию (HTTP %d)", resp.StatusCode)
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(raw, &token) != nil || token.AccessToken == "" {
		return errors.New("Keycloak не вернул служебный access token")
	}
	a.token = token.AccessToken
	return nil
}

func (a *adminAPI) do(ctx context.Context, method, path string, body any, expected ...int) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("административный запрос Keycloak: %w", err)
	}
	raw, err := readAdminResponse(resp)
	if err != nil {
		return nil, err
	}
	for _, status := range expected {
		if resp.StatusCode == status {
			return raw, nil
		}
	}
	return nil, fmt.Errorf("Keycloak вернул HTTP %d для %s", resp.StatusCode, path)
}

func readAdminResponse(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAdminResponse+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxAdminResponse {
		return nil, errors.New("ответ Keycloak слишком велик")
	}
	return raw, nil
}

func (a *adminAPI) ensureRealm(ctx context.Context, realm string) error {
	_, err := a.do(ctx, http.MethodPost, "/admin/realms", map[string]any{
		"realm": realm, "enabled": true, "displayName": "JustHPC Virt Manager",
		"displayNameHtml": "JustHPC Virt Manager", "internationalizationEnabled": true,
		"defaultLocale": "ru", "supportedLocales": []string{"ru", "en"},
	}, http.StatusCreated, http.StatusConflict)
	if err != nil {
		return err
	}
	// Repair the visible name and locale for realms created by an earlier
	// installer. Preserve the complete representation so an update never resets
	// an administrator's unrelated realm policy.
	path := "/admin/realms/" + url.PathEscape(realm)
	raw, err := a.do(ctx, http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		return err
	}
	var current map[string]any
	if json.Unmarshal(raw, &current) != nil {
		return errors.New("не удалось прочитать настройки realm Keycloak")
	}
	current["displayName"] = "JustHPC Virt Manager"
	current["displayNameHtml"] = "JustHPC Virt Manager"
	current["internationalizationEnabled"] = true
	current["defaultLocale"] = "ru"
	current["supportedLocales"] = []string{"ru", "en"}
	_, err = a.do(ctx, http.MethodPut, path, current, http.StatusNoContent)
	return err
}

func (a *adminAPI) ensureGroup(ctx context.Context, realm, group string) error {
	_, err := a.do(ctx, http.MethodPost, "/admin/realms/"+url.PathEscape(realm)+"/groups",
		map[string]string{"name": group}, http.StatusCreated, http.StatusConflict)
	return err
}

func (a *adminAPI) ensureServiceClient(ctx context.Context, clientID, secret string) error {
	path := "/admin/realms/master/clients"
	body := map[string]any{
		"clientId": clientID, "secret": secret, "enabled": true, "publicClient": false,
		"serviceAccountsEnabled": true, "standardFlowEnabled": false, "directAccessGrantsEnabled": false,
	}
	_, err := a.do(ctx, http.MethodPost, path, body, http.StatusCreated, http.StatusConflict)
	if err != nil {
		return err
	}
	raw, err := a.do(ctx, http.MethodGet, path+"?clientId="+url.QueryEscape(clientID)+"&search=true", nil, http.StatusOK)
	if err != nil {
		return err
	}
	var clients []map[string]any
	if json.Unmarshal(raw, &clients) != nil {
		return errors.New("не удалось прочитать служебный клиент Keycloak")
	}
	var clientUUID string
	for _, client := range clients {
		if client["clientId"] == clientID {
			if clientUUID != "" {
				return errors.New("в master realm несколько служебных клиентов helper")
			}
			clientUUID, _ = client["id"].(string)
		}
	}
	if clientUUID == "" {
		return errors.New("служебный клиент helper не найден после создания")
	}
	// PUT also repairs or deliberately rotates the secret if a previous first
	// attempt stopped between creating the client and saving root-only state.
	body["id"] = clientUUID
	if _, err := a.do(ctx, http.MethodPut, path+"/"+url.PathEscape(clientUUID), body, http.StatusNoContent); err != nil {
		return err
	}
	return nil
}

func (a *adminAPI) ensureRealmAdminAccess(ctx context.Context, realm, serviceClientID string) error {
	serviceUserID, err := a.serviceAccountUserID(ctx, serviceClientID)
	if err != nil {
		return err
	}
	realmClientUUID, err := a.exactClientUUID(ctx, "master", realm+"-realm")
	if err != nil {
		return fmt.Errorf("служебный клиент realm %s: %w", realm, err)
	}
	rolesRaw, err := a.do(ctx, http.MethodGet,
		"/admin/realms/master/clients/"+url.PathEscape(realmClientUUID)+"/roles?briefRepresentation=false&max=100", nil, http.StatusOK)
	if err != nil {
		return err
	}
	var roles []map[string]any
	if json.Unmarshal(rolesRaw, &roles) != nil || len(roles) == 0 {
		return fmt.Errorf("у служебного клиента realm %s не найдены административные роли", realm)
	}
	for _, role := range roles {
		if role["id"] == nil || role["name"] == nil {
			return fmt.Errorf("служебный клиент realm %s вернул некорректную роль", realm)
		}
	}
	_, err = a.do(ctx, http.MethodPost,
		"/admin/realms/master/users/"+url.PathEscape(serviceUserID)+"/role-mappings/clients/"+url.PathEscape(realmClientUUID),
		roles, http.StatusNoContent)
	return err
}

func (a *adminAPI) removeServiceClientMasterAdmin(ctx context.Context, serviceClientID string) error {
	serviceUserID, err := a.serviceAccountUserID(ctx, serviceClientID)
	if err != nil {
		return err
	}
	roleRaw, err := a.do(ctx, http.MethodGet, "/admin/realms/master/roles/admin", nil, http.StatusOK)
	if err != nil {
		return err
	}
	var role map[string]any
	if json.Unmarshal(roleRaw, &role) != nil || role["id"] == nil {
		return errors.New("в master realm не найдена роль admin")
	}
	_, err = a.do(ctx, http.MethodDelete,
		"/admin/realms/master/users/"+url.PathEscape(serviceUserID)+"/role-mappings/realm",
		[]map[string]any{role}, http.StatusNoContent)
	return err
}

func (a *adminAPI) serviceAccountUserID(ctx context.Context, serviceClientID string) (string, error) {
	serviceClientUUID, err := a.exactClientUUID(ctx, "master", serviceClientID)
	if err != nil {
		return "", err
	}
	serviceRaw, err := a.do(ctx, http.MethodGet,
		"/admin/realms/master/clients/"+url.PathEscape(serviceClientUUID)+"/service-account-user", nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	var serviceUser struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(serviceRaw, &serviceUser) != nil || serviceUser.ID == "" {
		return "", errors.New("у служебного клиента helper нет service account")
	}
	return serviceUser.ID, nil
}

func (a *adminAPI) verifyRealmAccess(ctx context.Context, realm string) error {
	_, err := a.do(ctx, http.MethodGet, "/admin/realms/"+url.PathEscape(realm), nil, http.StatusOK)
	return err
}

func (a *adminAPI) verifyMasterAdminRevoked(ctx context.Context, existingRealm string) error {
	_, err := a.do(ctx, http.MethodPost, "/admin/realms", map[string]any{
		"realm": existingRealm, "enabled": true,
	}, http.StatusForbidden)
	if err != nil {
		return fmt.Errorf("глобальная роль helper в master realm не отозвана: %w", err)
	}
	return nil
}

func (a *adminAPI) cleanupTemporaryPrincipals(ctx context.Context, currentBootstrapUser, currentRecoveryClient string) error {
	// The access token belongs either to the temporary bootstrap user or to the
	// temporary recovery service client. Deleting the principal that issued the
	// token invalidates it immediately, so the current principal must be the
	// very last administrative request in the cleanup sequence.
	if currentBootstrapUser != "" {
		if err := a.deleteClientsWithPrefix(ctx, "master", "kc-web-recovery-", ""); err != nil {
			return err
		}
		return a.deleteUsersWithPrefix(ctx, "master", "kc-web-bootstrap-", currentBootstrapUser)
	}
	if currentRecoveryClient != "" {
		if err := a.deleteUsersWithPrefix(ctx, "master", "kc-web-bootstrap-", ""); err != nil {
			return err
		}
		return a.deleteClientsWithPrefix(ctx, "master", "kc-web-recovery-", currentRecoveryClient)
	}
	if err := a.deleteUsersWithPrefix(ctx, "master", "kc-web-bootstrap-", ""); err != nil {
		return err
	}
	return a.deleteClientsWithPrefix(ctx, "master", "kc-web-recovery-", "")
}

func (a *adminAPI) deleteUsersWithPrefix(ctx context.Context, realm, prefix, currentLast string) error {
	path := "/admin/realms/" + url.PathEscape(realm) + "/users"
	raw, err := a.do(ctx, http.MethodGet,
		path+"?search="+url.QueryEscape(prefix)+"&first=0&max=1000", nil, http.StatusOK)
	if err != nil {
		return err
	}
	var users []struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	if json.Unmarshal(raw, &users) != nil {
		return errors.New("не удалось прочитать временных администраторов Keycloak")
	}
	deleteUser := func(user struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}) error {
		if user.ID == "" {
			return errors.New("у временного администратора Keycloak нет ID")
		}
		_, err := a.do(ctx, http.MethodDelete, path+"/"+url.PathEscape(user.ID), nil, http.StatusNoContent)
		return err
	}
	var current *struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	for i := range users {
		user := &users[i]
		if !strings.HasPrefix(user.Username, prefix) {
			continue
		}
		if currentLast != "" && user.Username == currentLast {
			current = user
			continue
		}
		if err := deleteUser(*user); err != nil {
			return err
		}
	}
	if current != nil {
		return deleteUser(*current)
	}
	return nil
}

func (a *adminAPI) deleteClientsWithPrefix(ctx context.Context, realm, prefix, currentLast string) error {
	path := "/admin/realms/" + url.PathEscape(realm) + "/clients"
	raw, err := a.do(ctx, http.MethodGet,
		path+"?clientId="+url.QueryEscape(prefix)+"&search=true&first=0&max=1000", nil, http.StatusOK)
	if err != nil {
		return err
	}
	var clients []struct {
		ID       string `json:"id"`
		ClientID string `json:"clientId"`
	}
	if json.Unmarshal(raw, &clients) != nil {
		return errors.New("не удалось прочитать временные recovery-клиенты Keycloak")
	}
	deleteClient := func(client struct {
		ID       string `json:"id"`
		ClientID string `json:"clientId"`
	}) error {
		if client.ID == "" {
			return errors.New("у временного recovery-клиента Keycloak нет ID")
		}
		_, err := a.do(ctx, http.MethodDelete, path+"/"+url.PathEscape(client.ID), nil, http.StatusNoContent)
		return err
	}
	var current *struct {
		ID       string `json:"id"`
		ClientID string `json:"clientId"`
	}
	for i := range clients {
		client := &clients[i]
		if !strings.HasPrefix(client.ClientID, prefix) {
			continue
		}
		if currentLast != "" && client.ClientID == currentLast {
			current = client
			continue
		}
		if err := deleteClient(*client); err != nil {
			return err
		}
	}
	if current != nil {
		return deleteClient(*current)
	}
	return nil
}

func (a *adminAPI) exactClientUUID(ctx context.Context, realm, clientID string) (string, error) {
	path := "/admin/realms/" + url.PathEscape(realm) + "/clients"
	raw, err := a.do(ctx, http.MethodGet, path+"?clientId="+url.QueryEscape(clientID)+"&search=true", nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	var clients []map[string]any
	if json.Unmarshal(raw, &clients) != nil {
		return "", fmt.Errorf("не удалось прочитать клиент Keycloak %s", clientID)
	}
	found := ""
	for _, client := range clients {
		if client["clientId"] != clientID {
			continue
		}
		id, _ := client["id"].(string)
		if id == "" || found != "" {
			return "", fmt.Errorf("клиент Keycloak %s неоднозначен", clientID)
		}
		found = id
	}
	if found == "" {
		return "", fmt.Errorf("клиент Keycloak %s не найден", clientID)
	}
	return found, nil
}

func (a *adminAPI) secureRealm(ctx context.Context, realm string) error {
	base := "/admin/realms/" + url.PathEscape(realm)
	flow := "jhvirt-browser-mfa-v1"
	_, err := a.do(ctx, http.MethodPost, base+"/authentication/flows", map[string]any{
		"alias": flow, "description": "Password and mandatory OTP", "providerId": "basic-flow",
		"topLevel": true, "builtIn": false,
	}, http.StatusCreated, http.StatusConflict)
	if err != nil {
		return err
	}
	flowPath := base + "/authentication/flows/" + url.PathEscape(flow)
	for _, provider := range []string{"auth-username-password-form", "auth-otp-form"} {
		executions, err := a.flowExecutions(ctx, flowPath)
		if err != nil {
			return err
		}
		id := executionID(executions, provider)
		if id == "" {
			if _, err := a.do(ctx, http.MethodPost, flowPath+"/executions/execution",
				map[string]string{"provider": provider}, http.StatusCreated); err != nil {
				return err
			}
			executions, err = a.flowExecutions(ctx, flowPath)
			if err != nil {
				return err
			}
			id = executionID(executions, provider)
		}
		if id == "" {
			return fmt.Errorf("в browser flow не создан %s", provider)
		}
		if _, err := a.do(ctx, http.MethodPut, flowPath+"/executions",
			map[string]string{"id": id, "requirement": "REQUIRED"}, http.StatusNoContent); err != nil {
			return err
		}
	}
	_, err = a.do(ctx, http.MethodPut, base, map[string]any{
		"browserFlow": flow, "sslRequired": "external", "bruteForceProtected": true,
		"failureFactor": 5, "waitIncrementSeconds": 60, "maxFailureWaitSeconds": 900,
	}, http.StatusNoContent)
	return err
}

func (a *adminAPI) flowExecutions(ctx context.Context, flowPath string) ([]map[string]any, error) {
	raw, err := a.do(ctx, http.MethodGet, flowPath+"/executions", nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	if json.Unmarshal(raw, &out) != nil {
		return nil, errors.New("не удалось прочитать browser flow Keycloak")
	}
	return out, nil
}

func executionID(executions []map[string]any, provider string) string {
	for _, execution := range executions {
		if execution["providerId"] == provider {
			id, _ := execution["id"].(string)
			return id
		}
	}
	return ""
}
