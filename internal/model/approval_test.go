package model

import "testing"

// Каталог опасных действий — то место, где решается, что можно сделать в
// одиночку. Опечатка или пропуск здесь означают не сломанную сборку, а
// действие, прошедшее без согласования.
func TestGuardedActionCatalogIsSound(t *testing.T) {
	seen := map[GuardedAction]bool{}
	levels := map[ApprovalLevel]int{}

	for _, info := range GuardedActions() {
		if seen[info.Key] {
			t.Errorf("действие %q объявлено дважды", info.Key)
		}
		seen[info.Key] = true

		switch info.Level {
		case LevelQuorum, LevelVeto, LevelAudit:
			levels[info.Level]++
		default:
			t.Errorf("действие %q отнесено к неизвестному уровню %q", info.Key, info.Level)
		}

		if info.Title == "" {
			t.Errorf("действие %q без названия: согласующий увидит пустую строку", info.Key)
		}
		// Обоснование обязательно. Уровень действия однажды захотят понизить, и
		// решать это надо, читая причину, по которой его подняли.
		if info.Why == "" {
			t.Errorf("действие %q без объяснения, почему оно на этом уровне", info.Key)
		}
	}

	for _, level := range []ApprovalLevel{LevelQuorum, LevelVeto, LevelAudit} {
		if levels[level] == 0 {
			t.Errorf("уровню %q не отвечает ни одно действие", level)
		}
	}
}

// Самое разрушительное обязано остаться на кворуме. Тест фиксирует
// утверждённую матрицу: понизить эти действия можно только осознанно, поправив
// и его.
func TestMostDestructiveActionsRequireQuorum(t *testing.T) {
	for _, action := range []GuardedAction{
		GuardStorageDelete, GuardStorageRetarget, GuardAuditDisable, GuardPolicyUpdate,
	} {
		info, ok := GuardedActionByKey(action)
		if !ok {
			t.Errorf("действие %q пропало из каталога", action)
			continue
		}
		if info.Level != LevelQuorum {
			t.Errorf("действие %q ушло с кворума на %q", action, info.Level)
		}
	}
}

// Правка политики согласования обязана согласовываться сама. Иначе защиты нет
// вовсе: достаточно понизить уровень нужного действия и выполнить его в
// одиночку.
func TestPolicyUpdateIsGuarded(t *testing.T) {
	info, ok := GuardedActionByKey(GuardPolicyUpdate)
	if !ok {
		t.Fatal("правка политики согласования не входит в каталог")
	}
	if info.Level != LevelQuorum {
		t.Fatalf("правка политики на уровне %q, ожидался кворум", info.Level)
	}
}

// Истечение срока не должно выполнять действие: иначе достаточно дождаться
// отпуска согласующих.
func TestExpiredIsFinalAndNotExecuted(t *testing.T) {
	if !ApprovalExpired.Final() {
		t.Error("истёкшая заявка не считается отыгранной")
	}
	if ApprovalExpired == ApprovalExecuted {
		t.Error("истечение срока приравнено к выполнению")
	}
	for _, state := range []ApprovalState{ApprovalPending, ApprovalEscalated, ApprovalApproved, ApprovalScheduled} {
		if state.Final() {
			t.Errorf("состояние %q считается отыгранным, хотя заявка ещё в работе", state)
		}
	}
}

// Голоса считаются по людям, а не по нажатиям: повторное подтверждение тем же
// человеком не должно приближать кворум.
func TestVoteCounting(t *testing.T) {
	req := ApprovalRequest{
		Quorum: 2,
		Votes: []ApprovalVote{
			{Voter: "анна", Approve: true},
			{Voter: "борис", Approve: false},
		},
	}

	if got := req.Approvals(); got != 1 {
		t.Fatalf("голосов «за» %d, ожидался 1", got)
	}
	if !req.VotedBy("борис") {
		t.Error("голос против не засчитан как поданный")
	}
	if req.VotedBy("виктор") {
		t.Error("не голосовавший считается проголосовавшим")
	}
}

// Членство в группе — по именам учётных записей. Проверка должна быть точной:
// совпадение по части имени открыло бы согласование постороннему.
func TestGroupMembershipIsExact(t *testing.T) {
	group := ApprovalGroup{Members: []string{"анна", "борис"}}

	if !group.Has("анна") {
		t.Error("участник группы не распознан")
	}
	if group.Has("ан") || group.Has("аннаа") {
		t.Error("членство определяется по части имени")
	}
	if group.Has("") {
		t.Error("пустое имя считается участником группы")
	}
}
