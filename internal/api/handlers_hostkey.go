package api

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/sshtrust"
)

// hostKeyScanPayload asks for the key one host is currently presenting.
//
// The address comes from the form rather than from a saved record: the moment
// the operator needs a fingerprint is while adding a host, before there is
// anything to save.
type hostKeyScanPayload struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

type hostKeyScanResult struct {
	Line        string `json:"line"`
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`

	// Warning is shown next to the fingerprint, always.
	//
	// A scan proves nothing on its own: whoever could intercept the connection
	// could also answer this request. Presenting the result without saying so
	// would turn a convenience into a false sense of having verified something.
	Warning string `json:"warning"`
}

const hostKeyScanWarning = "Отпечаток получен по сети и сам по себе ничего не доказывает: " +
	"тот, кто способен вклиниться в соединение, ответил бы и на этот запрос. " +
	"Сверьте его со снятым на самом хосте: ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub"

// handleScanHostKey returns the SSH host key of an address, for confirmation.
//
// No credentials are involved: the handshake stops at the host key, before
// authentication. So the scan is safe to run against an address that has only
// been typed and not yet trusted.
func (s *Server) handleScanHostKey(w http.ResponseWriter, r *http.Request) {
	var payload hostKeyScanPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}

	host := strings.TrimSpace(payload.Host)
	if host == "" {
		s.writeError(w, r, badRequest("не указан адрес хоста"))
		return
	}
	port := payload.Port
	if port == 0 {
		port = 22
	}
	if port < 1 || port > 65535 {
		s.writeError(w, r, badRequest("порт вне диапазона: %d", port))
		return
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	key, err := sshtrust.Scan(r.Context(), addr, 15*time.Second)
	if err != nil {
		// Recorded as a failure, not silently: an operator probing addresses
		// through the service is worth seeing in the audit trail either way.
		s.audit(r, "host_key.scan", model.ScopeServer, addr, false, err.Error())
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	s.audit(r, "host_key.scan", model.ScopeServer, addr, true, key.Fingerprint)

	writeJSON(w, http.StatusOK, hostKeyScanResult{
		Line:        key.Line,
		Type:        key.Type,
		Fingerprint: key.Fingerprint,
		Warning:     hostKeyScanWarning,
	})
}

// auditHostKeyTrust records the decision to connect without verifying the host.
//
// Recorded when the setting is stored rather than on every connection: the
// useful question afterwards is who turned verification off and when, and a
// line per nightly connection would bury exactly that.
func (s *Server) auditHostKeyTrust(r *http.Request, scope model.Scope, objectID, name string,
	trustAny, applicable bool) {

	if !applicable || !trustAny {
		return
	}
	s.audit(r, "host_key.verification_disabled", scope, objectID, true, name+": "+hostKeyTrustDetail)
}

const hostKeyTrustDetail = "подключение выполняется без проверки ключа хоста"
