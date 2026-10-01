package setup

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Extract function definitions only: never source/execute the actual installer
// (which would affect services, Docker volumes and production databases).
func installerFunctions(t *testing.T, names ...string) string {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.ReplaceAll(string(raw), "\r\n", "\n")
	result := "set -eu\ndie() { echo \"$*\" >&2; exit 1; }\nsay() { printf '%s\\n' \"$*\"; }\nhave() { command -v \"$1\" >/dev/null 2>&1; }\n"
	for _, name := range names {
		start := strings.Index(source, "\n"+name+"() {\n")
		if start < 0 {
			t.Fatalf("missing installer function %s", name)
		}
		sourceFunction := source[start+1:]
		end := strings.Index(sourceFunction, "\n}\n")
		if end < 0 {
			t.Fatalf("missing end of %s", name)
		}
		result += sourceFunction[:end+3] + "\n"
	}
	return result
}

func installerShell(t *testing.T, source, input string, wantError bool) string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if runtime.GOOS == "windows" {
		sh = filepath.Join(os.Getenv("ProgramFiles"), "Git", "bin", "bash.exe")
		_, err = os.Stat(sh)
	}
	if err != nil {
		t.Skip("POSIX shell not available")
	}
	cmd := exec.Command(sh, "-c", source)
	cmd.Stdin = strings.NewReader(input)
	output, err := cmd.CombinedOutput()
	if (err != nil) != wantError {
		t.Fatalf("shell result %v, want failure %t:\n%s", err, wantError, output)
	}
	return string(output)
}

func TestInstallerUpdatePreservesPasswordLoginWithoutQuestions(t *testing.T) {
	source := installerFunctions(t, "choose_oidc")
	for _, state := range []string{"UPDATE_REQUESTED=1; MIGRATION_ACTIVE=0", "UPDATE_REQUESTED=0; MIGRATION_ACTIVE=1"} {
		output := installerShell(t, source+state+"; OIDC_MODE=''; choose_oidc; test \"$OIDC_MODE\" = none", "", false)
		if strings.Contains(output, "Как входить") {
			t.Fatal("update asked to redeploy login provider")
		}
	}
}

func TestInstallerUpdateLoadsKeycloakAndExternalOIDC(t *testing.T) {
	source := installerFunctions(t, "env_file_value", "load_existing_oidc", "choose_oidc")
	for _, profile := range []string{"keycloak", "metrics"} {
		// All test state is created by the fixture in its own temporary directory.
		script := `WORK=$(mktemp -d); trap 'rm -f "$WORK/.env"; rmdir "$WORK"' EXIT
printf '%s\n' 'JHV_OIDC_ENABLED=true' 'COMPOSE_PROFILES=` + profile + `' 'JHV_OIDC_ISSUER=https://kc/realms/existing' 'JHV_OIDC_CLIENT_ID=existing' 'JHV_OIDC_CLIENT_SECRET=test-only' > "$WORK/.env"
UPDATE_REQUESTED=1; MIGRATION_ACTIVE=0; OIDC_MODE=''; OIDC_EXISTING=0
OIDC_ISSUER=''; OIDC_BACKCHANNEL_URL=''; OIDC_CLIENT_ID=''; OIDC_ALLOW_LOCAL_LOGIN=''
KEYCLOAK_URL=''; KEYCLOAK_APP_ADMIN_USER_EXPLICIT=0; KEYCLOAK_PORT_EXPLICIT=0
KEYCLOAK_PORT=8081
KEYCLOAK_AD_PROVIDER_EXPLICIT=0; KEYCLOAK_AD_DOMAIN=''; KEYCLOAK_AD_CONTROLLER=''
KEYCLOAK_AD_URL=''; KEYCLOAK_AD_USERS_DN=''; KEYCLOAK_AD_GROUPS_DN=''; KEYCLOAK_AD_BIND_DN=''
load_existing_oidc; choose_oidc
test "$OIDC_EXISTING" = 1; test "$OIDC_CLIENT_ID" = existing
test "$OIDC_ISSUER" = https://kc/realms/existing
`
		mode := "external"
		if profile == "keycloak" {
			mode = "keycloak"
		}
		installerShell(t, source+script+"test \"$OIDC_MODE\" = "+mode, "", false)
	}
}

func TestInstallerRejectsUnsafeDataPaths(t *testing.T) {
	source := installerFunctions(t, "validate_data_path")
	for _, path := range []string{"/", "/.", "relative", "/srv/../root", "/srv/./backup", "/srv//backup", "/srv/bak$HOME", "/srv/back ups"} {
		installerShell(t, source+"validate_data_path '"+path+"'", "", true)
	}
	installerShell(t, source+"validate_data_path /srv/vm-backups", "", false)
}

func TestInstallerDataPathDialogDoesNotCapturePrompt(t *testing.T) {
	source := installerFunctions(t, "ask_data_dir")
	installerShell(t, source+`path=$(ask_data_dir JHV_BACKUP_DIR /missing); test "$path" = /srv/backups`, "/srv/backups\n", false)
}

func TestInstallerUpdateDoesNotAskToReconfigureAD(t *testing.T) {
	// Simulate a terminal while keeping deterministic scripted input. The
	// production terminal check itself is not mocked anywhere in install.sh.
	source := strings.ReplaceAll(installerFunctions(t, "prepare_keycloak_ad"), "[ -t 0 ]", "true")
	output := installerShell(t, source+`OIDC_MODE=keycloak; KEYCLOAK_AD_REQUESTED=0; UPDATE_REQUESTED=1; MIGRATION_ACTIVE=0
prepare_keycloak_ad`, "", false)
	if strings.Contains(output, "Настроить Active Directory") {
		t.Fatal("update asked to reconfigure AD")
	}
}

func TestInstallerReuseRequiresRepositoryAndNeverReplacesActiveKey(t *testing.T) {
	source := installerFunctions(t, "validate_data_path", "validate_backup_source")
	fixture := `WORK=$(mktemp -d); trap 'rm -f "$WORK/key"; rmdir "$WORK/jhvirt" "$WORK" 2>/dev/null || true' EXIT
docker_bundle_present() { return 1; }; systemd_install_present() { return 1; }
REUSE_BACKUPS=1; BACKUP_DIR_OVERRIDE="$WORK"; BACKUP_KEY_FILE=''
RESTORE_DIR_OVERRIDE=''; DR_BACKUP_DIR_OVERRIDE=''; MIGRATION_ACTIVE=0
`
	installerShell(t, source+fixture+"validate_backup_source", "", true)
	installerShell(t, source+fixture+"mkdir \"$WORK/jhvirt\"; validate_backup_source", "", false)
	fixture += `mkdir "$WORK/jhvirt"
printf '%s\n' 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=' > "$WORK/key"
BACKUP_KEY_FILE="$WORK/key"
`
	installerShell(t, source+fixture+"validate_backup_source", "", false)
	installerShell(t, source+fixture+"docker_bundle_present() { return 0; }; validate_backup_source", "", true)
	installerShell(t, source+fixture+"printf bad > \"$WORK/key\"; validate_backup_source", "", true)
}

func TestInstallerInteractiveReuseSelection(t *testing.T) {
	source := strings.ReplaceAll(installerFunctions(t, "choose_backup_source"), "[ -t 0 ]", "true")
	fixture := `UPDATE_REQUESTED=0; MIGRATION_ACTION=''; MODE=docker; PREFIX=/opt/jhvirt
BACKUP_DIR_OVERRIDE=''; BACKUP_KEY_FILE=''; REUSE_BACKUPS=0
docker_bundle_present() { return 1; }; systemd_install_present() { return 1; }
choose_backup_source
test "$REUSE_BACKUPS" = 1
test "$BACKUP_DIR_OVERRIDE" = /srv/backups
test "$BACKUP_KEY_FILE" = /root/old-secret.key
`
	installerShell(t, source+fixture, "2\n/srv/backups\n/root/old-secret.key\n", false)
}

func TestInstallerServiceArchiveSelection(t *testing.T) {
	source := installerFunctions(t, "service_archive_candidates", "ask_service_archive")
	fixture := `PREFIX=$(mktemp -d)
trap 'rm -f "$PREFIX/backups/update-a.tar.gz" "$PREFIX/backups/update-b.tar.gz"; rmdir "$PREFIX/backups" "$PREFIX"' EXIT
mkdir "$PREFIX/backups"
: > "$PREFIX/backups/update-a.tar.gz"; : > "$PREFIX/backups/update-b.tar.gz"
migration_ask_nonempty() { read -r ANSWER; }
ask_service_archive
test "$ANSWER" = "$PREFIX/backups/update-b.tar.gz"
`
	installerShell(t, source+fixture, "2\n", false)
}

func TestInstallerSystemdPathsAllowRestoreButNotReadOnlyBackupWrites(t *testing.T) {
	source := installerFunctions(t, "env_file_value", "systemd_write_paths")
	fixture := `ENV_FILE=$(mktemp); trap 'rm -f "$ENV_FILE"' EXIT
printf '%s\n' 'JHV_INSTALL_BACKUP_DIR=/srv/backups' 'JHV_INSTALL_STORAGE_READ_ONLY=true' 'JHV_BACKUP_TEMP_DIR=/srv/restores/.tmp' > "$ENV_FILE"
PREFIX=/opt/jhvirt; MIGRATION_ACTIVE=0; UNIT=/nonexistent-test-unit
RESTORE_DIR_OVERRIDE=/srv/restores
paths=$(systemd_write_paths)
case "$paths" in *'/srv/backups'*) exit 1 ;; esac
case "$paths" in *'/srv/restores/.tmp'*) ;; *) exit 1 ;; esac
case "$paths" in *' /srv/restores'*) ;; *) exit 1 ;; esac
`
	installerShell(t, source+fixture, "", false)
}
