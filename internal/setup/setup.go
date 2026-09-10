// Package setup contains installation-time parsers. It is part of the shipped
// binary so an offline installation does not need jq, Python or a YAML editor.
package setup

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	"go.yaml.in/yaml/v3"
)

func Run(args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("не задана операция setup")
	}
	op, args := args[0], args[1:]
	if op == "tls-check" {
		return checkTLS(args, out)
	}
	if op == "ldap-filter" {
		var b strings.Builder
		b.WriteString("(|")
		for _, group := range args {
			if err := validGroup(group); err != nil {
				return err
			}
			b.WriteString("(cn=" + ldapEscape(group) + ")")
		}
		fmt.Fprint(out, b.String()+")")
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(in, 8<<20))
	if err != nil {
		return err
	}
	if strings.HasPrefix(op, "config-") {
		return configOperation(op, args, raw, out)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return errors.New("неверный JSON в ответе установщику")
	}
	switch op {
	case "json-get":
		for _, key := range args {
			obj, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("JSON: нет объекта %s", key)
			}
			value = obj[key]
		}
		if value == nil {
			return nil
		}
		if str, ok := value.(string); ok {
			fmt.Fprint(out, str)
			return nil
		}
	case "json-find":
		if len(args) != 2 {
			return errors.New("json-find: поле и значение обязательны")
		}
		list, ok := value.([]any)
		if !ok {
			return errors.New("ожидался JSON-массив")
		}
		var found any
		for _, item := range list {
			m, ok := item.(map[string]any)
			if ok && m[args[0]] == args[1] {
				if found != nil {
					return fmt.Errorf("неоднозначный объект %s=%s", args[0], args[1])
				}
				found = item
			}
		}
		if found == nil {
			return nil
		}
		value = found
	case "json-prefix":
		if len(args) != 2 {
			return errors.New("json-prefix: поле и префикс обязательны")
		}
		list, ok := value.([]any)
		if !ok {
			return errors.New("ожидался JSON-массив")
		}
		for _, item := range list {
			m, ok := item.(map[string]any)
			name, nameOK := m[args[0]].(string)
			id, idOK := m["id"].(string)
			if !ok || !nameOK || !idOK || !strings.HasPrefix(name, args[1]) {
				continue
			}
			if strings.ContainsAny(id+name, " \t\r\n") {
				return errors.New("JSON содержит небезопасный идентификатор")
			}
			fmt.Fprintf(out, "%s %s\n", id, name)
		}
		return nil
	case "json-client":
		if len(args) != 1 {
			return errors.New("json-client: URL обязателен")
		}
		obj, ok := value.(map[string]any)
		if !ok {
			return errors.New("ожидался объект клиента")
		}
		public, err := url.Parse(args[0])
		if err != nil || public.Host == "" {
			return errors.New("неверный URL приложения")
		}
		obj["redirectUris"] = []string{args[0] + "/api/v1/auth/oidc/callback"}
		obj["webOrigins"] = []string{args[0]}
		obj["publicClient"] = false
		obj["standardFlowEnabled"] = true
		obj["directAccessGrantsEnabled"] = false
		attrs, _ := obj["attributes"].(map[string]any)
		if attrs == nil {
			attrs = map[string]any{}
		}
		attrs["post.logout.redirect.uris"] = args[0] + "/login"
		attrs["pkce.code.challenge.method"] = "S256"
		obj["attributes"] = attrs
		mappers, _ := obj["protocolMappers"].([]any)
		mapper := map[string]any{"name": "groups", "protocol": "openid-connect", "protocolMapper": "oidc-group-membership-mapper", "config": map[string]any{
			"claim.name": "groups", "full.path": "false", "id.token.claim": "true", "access.token.claim": "true", "userinfo.token.claim": "true",
		}}
		replaced := false
		for i, m := range mappers {
			old, ok := m.(map[string]any)
			if ok && old["name"] == "groups" {
				if replaced {
					return errors.New("несколько groups mappers")
				}
				if id, ok := old["id"]; ok {
					mapper["id"] = id
				}
				mappers[i] = mapper
				replaced = true
			}
		}
		if !replaced {
			mappers = append(mappers, mapper)
		}
		obj["protocolMappers"] = mappers
	case "json-groups":
		list, ok := value.([]any)
		if !ok {
			return errors.New("ожидался список групп")
		}
		for _, expected := range args {
			count := 0
			for _, item := range list {
				m, ok := item.(map[string]any)
				if ok && m["name"] == expected {
					count++
				}
			}
			if count != 1 {
				return fmt.Errorf("группа %q: найдено %d, требуется ровно одна", expected, count)
			}
		}
		return nil
	default:
		return fmt.Errorf("неизвестная операция setup: %s", op)
	}
	return json.NewEncoder(out).Encode(value)
}

func validGroup(s string) error {
	if strings.TrimSpace(s) == "" || len(s) > 256 || strings.ContainsFunc(s, unicode.IsControl) {
		return errors.New("имя группы пусто, слишком длинное или содержит управляющие символы")
	}
	return nil
}
func ldapEscape(s string) string {
	return strings.NewReplacer("\\", `\5c`, "*", `\2a`, "(", `\28`, ")", `\29`, "\x00", `\00`).Replace(s)
}

func configOperation(op string, args []string, raw []byte, out io.Writer) error {
	var document yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&document); err != nil {
		return fmt.Errorf("YAML: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("ожидается один YAML-документ")
	}
	// Decode once to reject duplicate keys and incompatible role values before
	// touching the node tree. Unknown configuration fields remain untouched.
	var checked map[string]any
	if err := document.Decode(&checked); err != nil {
		return err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return errors.New("ожидается YAML-объект")
	}
	if op == "config-get" {
		var value any = checked
		for _, key := range args {
			m, ok := value.(map[string]any)
			if !ok {
				return nil
			}
			value = m[key]
		}
		if value == nil {
			return nil
		}
		if str, ok := value.(string); ok {
			fmt.Fprint(out, str)
			return nil
		}
		return json.NewEncoder(out).Encode(value)
	}
	oidc, err := mappingPath(document.Content[0], "auth", "oidc")
	if err != nil {
		return err
	}
	roles, err := mappingPath(oidc, "role_mapping")
	if err != nil {
		return err
	}
	if op == "config-groups" {
		var existing map[string]string
		if err := roles.Decode(&existing); err != nil {
			return err
		}
		selected := map[string]string{}
		keys := make([]string, 0, len(existing))
		for k := range existing {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, g := range keys {
			role := existing[g]
			if role != "admin" && role != "operator" && role != "viewer" {
				continue
			}
			if selected[role] != "" {
				return fmt.Errorf("для роли %s задано несколько групп; настройте AD вручную без замены role_mapping", role)
			}
			selected[role] = g
		}
		return json.NewEncoder(out).Encode(selected)
	}
	if op != "config-init-roles" || len(args) != 3 {
		return errors.New("config-init-roles требует три группы")
	}
	if len(roles.Content) > 0 {
		_, err := out.Write(raw)
		return err
	}
	seen := map[string]bool{}
	for i, g := range args {
		if err := validGroup(g); err != nil {
			return err
		}
		key := strings.ToLower(strings.TrimSpace(g))
		if seen[key] {
			return errors.New("группы разных ролей должны различаться")
		}
		seen[key] = true
		roles.Content = append(roles.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: g}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: []string{"admin", "operator", "viewer"}[i]})
	}
	enc := yaml.NewEncoder(out)
	enc.SetIndent(2)
	defer enc.Close()
	return enc.Encode(&document)
}

func mappingPath(node *yaml.Node, keys ...string) (*yaml.Node, error) {
	for _, key := range keys {
		if node.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s: требуется YAML mapping без alias", key)
		}
		var found *yaml.Node
		for i := 0; i < len(node.Content); i += 2 {
			if node.Content[i].Value == key {
				found = node.Content[i+1]
				break
			}
		}
		if found == nil {
			found = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, found)
		}
		node = found
	}
	if node.Kind != yaml.MappingNode {
		return nil, errors.New("ожидается YAML mapping без alias")
	}
	return node, nil
}

func checkTLS(args []string, out io.Writer) error {
	if len(args) != 2 {
		return errors.New("tls-check: LDAPS URL и PEM CA обязательны")
	}
	pem, err := os.ReadFile(args[1])
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return errors.New("не удалось прочитать CA")
	}
	for _, raw := range strings.Fields(args[0]) {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "ldaps" || u.Hostname() == "" || u.Port() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("требуется ldaps://FQDN:порт без пути и учётных данных")
		}
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", u.Host, &tls.Config{RootCAs: roots, ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
		if err != nil {
			return fmt.Errorf("LDAPS %s: DNS/порт/сертификат: %w", u.Host, err)
		}
		conn.Close()
		fmt.Fprintf(out, "    LDAPS %s: DNS, соединение и сертификат проверены\n", u.Host)
	}
	return nil
}
