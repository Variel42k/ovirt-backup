package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/proxmox"
	"github.com/Variel42k/ovirt-backup/internal/sshtrust"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// hostKeyScanPayload запрашивает ключ, который в настоящее время представляет один из
// хостов.
//
// Адрес приходит из формы, а не из сохраненной записи: в тот момент, когда оператору нужен
// отпечаток пальца, он добавляет хост, прежде чем что-то сохранить.
type hostKeyScanPayload struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

type hostKeyScanResult struct {
	Line        string `json:"line"`
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`

	// Предостережение всегда появляется рядом с отпечатком пальца.
	//
	// Сканирование само по себе ничего не доказывает: тот, кто может перехватить
	// соединение, также может ответить на этот запрос. Представление результата, не говоря
	// об этом, превратило бы удобство в false чувство проверки чего-либо.
	Warning string `json:"warning"`
}

type proxmoxNodeKey struct {
	Node        string `json:"node"`
	Address     string `json:"address"`
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
}

type proxmoxNodeKeysResult struct {
	Bundle  string           `json:"bundle"`
	Keys    []proxmoxNodeKey `json:"keys"`
	Warning string           `json:"warning"`
}

// handleScanProxmoxHostKeys получает список узлов через доверенное соединение API и
// считывает их SSH-ключи до аутентификации. Требуем ответ от всех узлов, чтобы после
// миграции ВМ не потеряла защиту из-за незакреплённого ключа.
func (s *Server) handleScanProxmoxHostKeys(w http.ResponseWriter, r *http.Request) {
	var payload serverPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	if payload.Password == "" || (payload.CACert == "" && !payload.ClearCACert) {
		var existing *model.Server
		var err error
		if payload.ID != "" {
			existing, err = s.store.GetServer(r.Context(), payload.ID)
		} else if payload.Name != "" {
			existing, err = s.store.GetServerByName(r.Context(), payload.Name)
		}
		if err == nil && existing != nil {
			payload.fillHiddenFrom(existing)
		}
	}
	if model.ServerKind(payload.Kind) != model.KindProxmox {
		s.writeError(w, r, badRequest("получение ключей кластера доступно только для Proxmox"))
		return
	}
	client, err := proxmox.New(proxmox.Config{BaseURL: payload.EngineURL, TokenID: payload.Username,
		TokenSecret: payload.Password, CACert: payload.CACert, InsecureTLS: payload.InsecureTLS,
		Timeout: 25 * time.Second})
	if err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	inv, err := client.FetchInventory(ctx, "")
	if err != nil {
		s.writeError(w, r, badRequest("инвентарь Proxmox: %v", err))
		return
	}
	if len(inv.Hosts) == 0 {
		s.writeError(w, r, badRequest("Proxmox не вернул ни одного узла кластера"))
		return
	}
	port := payload.SSHPort
	if port == 0 {
		port = 22
	}
	if port < 1 || port > 65535 {
		s.writeError(w, r, badRequest("порт SSH вне диапазона: %d", port))
		return
	}
	type scanned struct {
		result proxmoxNodeKey
		line   string
		err    error
	}
	results := make(chan scanned, len(inv.Hosts))
	var wg sync.WaitGroup
	for _, item := range inv.Hosts {
		host := item.Address
		if strings.TrimSpace(host) == "" {
			host = item.Name
		}
		wg.Add(1)
		go func(node, address string) {
			defer wg.Done()
			addr := net.JoinHostPort(address, strconv.Itoa(port))
			key, scanErr := sshtrust.Scan(ctx, addr, 15*time.Second)
			if scanErr != nil {
				results <- scanned{result: proxmoxNodeKey{Node: node, Address: address}, err: scanErr}
				return
			}
			publicKey, _, _, _, parseErr := ssh.ParseAuthorizedKey([]byte(key.Line))
			if parseErr != nil {
				results <- scanned{result: proxmoxNodeKey{Node: node, Address: address},
					err: fmt.Errorf("разбор полученного host key: %w", parseErr)}
				return
			}
			results <- scanned{result: proxmoxNodeKey{Node: node, Address: address,
				Type: key.Type, Fingerprint: key.Fingerprint}, line: knownhosts.Line([]string{addr}, publicKey)}
		}(item.Name, host)
	}
	wg.Wait()
	close(results)
	var items []scanned
	for item := range results {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].result.Node < items[j].result.Node })
	var failures, lines []string
	response := proxmoxNodeKeysResult{Warning: hostKeyScanWarning}
	for _, item := range items {
		if item.err != nil {
			failures = append(failures, fmt.Sprintf("%s (%s): %v", item.result.Node, item.result.Address, item.err))
			continue
		}
		response.Keys = append(response.Keys, item.result)
		lines = append(lines, strings.TrimSpace(item.line))
	}
	if len(failures) > 0 {
		err := fmt.Errorf("не получены ключи всех узлов: %s", strings.Join(failures, "; "))
		s.audit(r, "proxmox_host_keys.scan", model.ScopeServer, payload.Name, false, err.Error())
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	response.Bundle = strings.Join(lines, "\n")
	s.audit(r, "proxmox_host_keys.scan", model.ScopeServer, payload.Name, true,
		fmt.Sprintf("узлов: %d", len(response.Keys)))
	writeJSON(w, http.StatusOK, response)
}

const hostKeyScanWarning = "Отпечаток получен по сети и сам по себе ничего не доказывает: " +
	"тот, кто способен вклиниться в соединение, ответил бы и на этот запрос. " +
	"Сверьте его со снятым на самом хосте: ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub"

// handleScanHostKey возвращает ключ SSH узла адреса, для подтверждения.
//
// Учетные данные аутентификации не используются: рукопожатие останавливается на ключе хоста
// перед аутентификацией. Таким образом, сканирование безопасно для запуска по адресу,
// который был только введен и еще не доверен.
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
		// Записано как сбой, а не молча: оператор, проверяющий адреса через сервис, в любом
		// случае стоит увидеть в аудиторском следе.
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

// auditHostKeyTrust Записывает решение о подключении без проверки хоста.
//
// Записывается, когда параметр сохраняется, а не на каждом соединении: полезный вопрос
// после этого заключается в том, кто и когда выключил верификацию, и строка за ночное
// соединение похоронит именно это.
func (s *Server) auditHostKeyTrust(r *http.Request, scope model.Scope, objectID, name string,
	trustAny, applicable bool) {

	if !applicable || !trustAny {
		return
	}
	s.audit(r, "host_key.verification_disabled", scope, objectID, true, name+": "+hostKeyTrustDetail)
}

const hostKeyTrustDetail = "подключение выполняется без проверки ключа хоста"
