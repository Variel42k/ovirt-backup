package model

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// alertKindConst выхватывает объявления вида AlertXxx = "kind" из monitor.go.
var alertKindConst = regexp.MustCompile(`(?m)^\s*(Alert[A-Za-z]+)\s*=\s*"([a-z_]+)"`)

// Каждый существующий тип оповещения должен быть разложен по адресату.
//
// Список берётся из исходника, а не повторяется здесь руками. Повторённый
// список — это второй перечень типов, и забыть про него так же легко, как про
// саму раскладку: тогда новый вид оповещения молча уходил бы в ленту службы,
// и человек, которому он адресован, узнавал бы о проблеме от кого-то другого.
func TestEveryAlertKindHasAudience(t *testing.T) {
	source, err := os.ReadFile("monitor.go")
	if err != nil {
		t.Fatalf("чтение monitor.go: %v", err)
	}

	matches := alertKindConst.FindAllStringSubmatch(string(source), -1)
	if len(matches) < 20 {
		t.Fatalf("в monitor.go нашлось всего %d типов оповещений — "+
			"похоже, разбор сломался, а не список опустел", len(matches))
	}

	for _, m := range matches {
		name, kind := m[1], m[2]
		// Состояния оповещения объявлены рядом теми же именами Alert*, но это
		// не типы: у них свой тип AlertState.
		if strings.HasPrefix(kind, "firing") || strings.HasPrefix(kind, "acked") ||
			strings.HasPrefix(kind, "resolved") {
			continue
		}
		if _, ok := alertAudienceByKind[kind]; !ok {
			t.Errorf("тип %s (%q) не разложен по адресатам: "+
				"добавьте его в alertAudienceByKind", name, kind)
		}
	}
}

// В раскладке не должно быть типов, которых больше нет в коде: они означают,
// что оповещение переименовали, а раскладку не поправили — и настоящий тип
// остался без адресата.
func TestAudienceMapHasNoStrayKinds(t *testing.T) {
	source, err := os.ReadFile("monitor.go")
	if err != nil {
		t.Fatalf("чтение monitor.go: %v", err)
	}
	text := string(source)

	for kind := range alertAudienceByKind {
		if !strings.Contains(text, `"`+kind+`"`) {
			t.Errorf("в раскладке есть %q, которого нет среди типов оповещений", kind)
		}
	}
}

// Адресат должен быть одним из объявленных: опечатка в раскладке даёт ленту,
// которую невозможно выбрать в интерфейсе.
func TestAudiencesAreKnown(t *testing.T) {
	known := map[AlertAudience]bool{}
	for _, info := range AlertAudiences() {
		if info.Title == "" || info.Description == "" {
			t.Errorf("адресат %q без названия или описания", info.Key)
		}
		known[info.Key] = true
	}

	for kind, audience := range alertAudienceByKind {
		if !known[audience] {
			t.Errorf("тип %q адресован неизвестному получателю %q", kind, audience)
		}
	}
}

// Незнакомый тип не должен теряться: он достаётся службе.
func TestUnknownKindFallsBackToService(t *testing.T) {
	if got := AudienceOf("совершенно новый тип"); got != AudienceService {
		t.Fatalf("незнакомый тип отдан %q, ожидалась служба", got)
	}
}

// Раскладка должна покрывать всех объявленных адресатов: адресат без единого
// типа — это пустая вкладка в интерфейсе.
func TestEveryAudienceHasKinds(t *testing.T) {
	counts := map[AlertAudience]int{}
	for _, audience := range alertAudienceByKind {
		counts[audience]++
	}
	for _, info := range AlertAudiences() {
		if counts[info.Key] == 0 {
			t.Errorf("адресату %q не отвечает ни один тип оповещения", info.Key)
		}
	}
}
