package model

import "time"

// Согласование опасных действий.
//
// Смысл в том, что утечка одной учётной записи не должна давать возможности
// уничтожить копии. Пароль администратора рано или поздно утекает — через
// фишинг, через забытую сессию, через уволившегося сотрудника, — и всё, что
// стоит между этим и потерей данных, это требование второй подписи.
//
// Мера раздражающая, поэтому уровней три, а не один: дорогое согласование
// имеет смысл там, где действие необратимо и массово, и вредит там, где оно
// рутинно. Разложить действия по уровням важнее, чем сам механизм.

// ApprovalLevel — насколько дорого согласование действия.
type ApprovalLevel string

const (
	// LevelQuorum — до выполнения нужно несколько подтверждений от разных
	// людей. Для того, что нельзя отменить и что уничтожает данные целиком.
	LevelQuorum ApprovalLevel = "quorum"
	// LevelVeto — одного подтверждения достаточно, но выполнение откладывается,
	// и в течение окна любой из группы может отменить. Для рутинного и
	// разрушительного одновременно: массового удаления по расписанию.
	LevelVeto ApprovalLevel = "veto"
	// LevelAudit — выполняется сразу, запись в журнале аудита. Для того, что
	// затрагивает один объект и восстановимо.
	LevelAudit ApprovalLevel = "audit"
)

// GuardedAction — действие, проходящее через согласование.
//
// Имена совпадают с теми, что пишутся в журнал аудита: разбирая инцидент,
// человек читает один и тот же идентификатор в заявке и в журнале, а не
// сопоставляет два словаря.
type GuardedAction string

const (
	GuardStorageDelete   GuardedAction = "storage.delete"
	GuardStorageRetarget GuardedAction = "storage.retarget"
	GuardAuditDisable    GuardedAction = "audit.disable"
	GuardPolicyUpdate    GuardedAction = "approval.policy.update"

	GuardRetentionApply GuardedAction = "retention.apply"
	GuardJobDelete      GuardedAction = "job.delete"
	GuardServerDelete   GuardedAction = "server.delete"

	GuardBackupDelete      GuardedAction = "backup.delete"
	GuardRemediationManual GuardedAction = "remediation.manual"
)

// GuardedActionInfo описывает действие для того, кто настраивает политику.
type GuardedActionInfo struct {
	Key   GuardedAction `json:"key"`
	Title string        `json:"title"`
	// Level — уровень по умолчанию. Политика может его изменить.
	Level ApprovalLevel `json:"level"`
	// Why объясняет, почему действие отнесено именно к этому уровню.
	Why string `json:"why"`
}

// guardedActions — раскладка опасных действий по уровням.
//
// Утверждена отдельно от кода и меняется осознанно: перенос действия на
// уровень ниже — это решение о том, что его можно совершить в одиночку.
var guardedActions = []GuardedActionInfo{
	{GuardStorageDelete, "Удаление хранилища копий", LevelQuorum,
		"вместе с хранилищем перестают быть доступны все лежащие в нём копии"},
	{GuardStorageRetarget, "Смена адреса или учётных данных хранилища", LevelQuorum,
		"подмена адреса уводит новые копии в чужое место, а старые делает недоступными"},
	{GuardAuditDisable, "Отключение вывода журнала аудита", LevelQuorum,
		"первое, что делают после захвата, — прекращают запись следов"},
	{GuardPolicyUpdate, "Правка политики согласования", LevelQuorum,
		"иначе достаточно снизить уровень нужного действия и выполнить его в одиночку"},

	{GuardRetentionApply, "Применение политики хранения", LevelVeto,
		"удаляет копии пачками по расписанию; ошибка в политике стирает историю целиком"},
	{GuardJobDelete, "Удаление задания бэкапа", LevelVeto,
		"машины остаются без копий молча — расписание просто перестаёт срабатывать"},
	{GuardServerDelete, "Удаление подключения к движку", LevelVeto,
		"вместе с ним теряются задания и связь с уже сделанными копиями"},

	{GuardBackupDelete, "Удаление одной копии", LevelAudit,
		"затрагивает один объект, остальная цепочка остаётся"},
	{GuardRemediationManual, "Ручное восстановительное действие", LevelAudit,
		"выполняется по живой аварии, и задержка на согласование опаснее самого действия"},
}

// GuardedActions возвращает каталог действий.
func GuardedActions() []GuardedActionInfo { return guardedActions }

// GuardedActionByKey ищет действие в каталоге.
func GuardedActionByKey(key GuardedAction) (GuardedActionInfo, bool) {
	for _, info := range guardedActions {
		if info.Key == key {
			return info, true
		}
	}
	return GuardedActionInfo{}, false
}

// ApprovalGroup — группа согласующих.
type ApprovalGroup struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Title string `json:"title"`
	// Members — имена учётных записей. Роли сюда намеренно не входят: членство
	// по роли означало бы, что выдача роли молча меняет состав согласующих, а
	// это ровно то, что должно требовать согласования.
	Members   []string  `json:"members"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Has сообщает, входит ли учётная запись в группу.
func (g ApprovalGroup) Has(username string) bool {
	for _, member := range g.Members {
		if member == username {
			return true
		}
	}
	return false
}

// ApprovalPolicy — как согласуется одно действие.
type ApprovalPolicy struct {
	Action GuardedAction `json:"action"`
	Level  ApprovalLevel `json:"level"`
	// Quorum — сколько подтверждений нужно, не считая инициатора.
	Quorum int `json:"quorum"`
	// GroupName — основная группа согласующих.
	GroupName string `json:"group_name"`
	// FallbackGroupName — резервная. К ней переходят, если основная не собрала
	// кворум за Timeout: администраторы уходят в отпуск, а хранилище иногда
	// нужно удалить именно на этой неделе.
	FallbackGroupName string `json:"fallback_group_name,omitempty"`
	// Timeout — сколько ждать кворума до эскалации на резервную группу.
	Timeout time.Duration `json:"timeout"`
	// VetoWindow — окно отмены для уровня veto.
	VetoWindow time.Duration `json:"veto_window"`
}

// ApprovalState — состояние заявки.
type ApprovalState string

const (
	ApprovalPending   ApprovalState = "pending"
	ApprovalEscalated ApprovalState = "escalated"
	ApprovalApproved  ApprovalState = "approved"
	ApprovalScheduled ApprovalState = "scheduled"
	ApprovalRejected  ApprovalState = "rejected"
	ApprovalVetoed    ApprovalState = "vetoed"
	// ApprovalExpired — кворум не собран в срок. Действие НЕ выполняется:
	// иначе истечение срока стало бы способом провести что угодно, дождавшись
	// отпуска согласующих.
	ApprovalExpired  ApprovalState = "expired"
	ApprovalExecuted ApprovalState = "executed"
)

// Final сообщает, что заявка отыграна и больше не изменится.
func (s ApprovalState) Final() bool {
	switch s {
	case ApprovalRejected, ApprovalVetoed, ApprovalExpired, ApprovalExecuted:
		return true
	default:
		return false
	}
}

// ApprovalRequest — заявка на выполнение опасного действия.
type ApprovalRequest struct {
	ID     string        `json:"id"`
	Action GuardedAction `json:"action"`
	// ObjectID и ObjectName — над чем действие совершается.
	ObjectID   string `json:"object_id,omitempty"`
	ObjectName string `json:"object_name,omitempty"`
	// Summary — что именно произойдёт, словами. Согласующий видит его, а не
	// идентификаторы: подтверждать то, чего не понимаешь, — худший вид
	// согласования.
	Summary   string `json:"summary"`
	Requester string `json:"requester"`
	// Reason — обоснование от инициатора. Обязательно: заявка без объяснения
	// подтверждается не глядя.
	Reason string `json:"reason"`

	State     ApprovalState `json:"state"`
	Level     ApprovalLevel `json:"level"`
	Quorum    int           `json:"quorum"`
	GroupName string        `json:"group_name"`
	// Escalated — заявка уже перешла к резервной группе.
	Escalated bool `json:"escalated"`

	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	DecidedAt *time.Time `json:"decided_at,omitempty"`
	// ExecuteAfter заполняется на уровне veto: до этого момента действие можно
	// отменить.
	ExecuteAfter *time.Time `json:"execute_after,omitempty"`
	// Payload — параметры действия, без которых его не воспроизвести.
	//
	// Нужен уровню veto: там действие выполняется само, когда окно отмены
	// вышло, и человека, который повторил бы запрос, рядом нет. Для удаления
	// хватает ObjectID, а применению политики хранения нужны ещё сервер, ВМ,
	// хранилище и сама политика.
	//
	// Секретам здесь не место. Заявка живёт в базе и уходит в оповещения — то
	// есть всё, что сюда попало, оказывается сразу в двух местах, откуда это
	// не отозвать. Действия, которым для повтора нужен секрет, автоматически
	// не выполняются: их доводит человек.
	Payload []byte `json:"-"`
	// Error — почему автоматическое выполнение не удалось.
	//
	// Заявка при этом не закрывается: молча пропавшее действие, которое все
	// считают выполненным, хуже отказа. Оператор видит причину и решает сам.
	Error string         `json:"error,omitempty"`
	Votes []ApprovalVote `json:"votes,omitempty"`
}

// ApprovalVote — один голос.
type ApprovalVote struct {
	RequestID string `json:"request_id"`
	// Voter — чей это голос. При делегировании здесь остаётся имя того, кто
	// передал право: кворум считается по нему, иначе делегат с двумя
	// делегированиями закрыл бы кворум в одиночку.
	Voter string `json:"voter"`
	// CastBy — кто фактически нажал кнопку. Пусто, когда голосовал сам.
	CastBy  string    `json:"cast_by,omitempty"`
	Approve bool      `json:"approve"`
	Comment string    `json:"comment,omitempty"`
	At      time.Time `json:"at"`
}

// Approvals считает голоса «за».
func (r ApprovalRequest) Approvals() int {
	n := 0
	for _, vote := range r.Votes {
		if vote.Approve {
			n++
		}
	}
	return n
}

// VotedBy сообщает, голосовал ли уже этот человек.
func (r ApprovalRequest) VotedBy(username string) bool {
	for _, vote := range r.Votes {
		if vote.Voter == username {
			return true
		}
	}
	return false
}

// BreakGlassEvent — выполнение в обход согласования.
//
// Запрещать аварийный доступ нельзя: бывает, что согласующие недоступны, а
// действие нужно сейчас. Смысл в том, что тихо им воспользоваться невозможно —
// событие уходит всем согласующим и в журнал аудита отдельной пометкой.
type BreakGlassEvent struct {
	ID       string        `json:"id"`
	Actor    string        `json:"actor"`
	Action   GuardedAction `json:"action"`
	ObjectID string        `json:"object_id,omitempty"`
	// Reason обязателен и длиннее одного слова: аварийный доступ без внятного
	// объяснения неотличим от злоупотребления им.
	Reason   string    `json:"reason"`
	Notified []string  `json:"notified,omitempty"`
	At       time.Time `json:"at"`
}

// ApprovalDelegation — переданное на время право голоса.
//
// Согласующий уходит в отпуск, кворум перестаёт собираться, и работа встаёт.
// Резервная группа решает это лишь отчасти: она включается по таймауту, то
// есть после того, как заявка уже провисела положенное время. Делегирование
// закрывает случай, когда отсутствие известно заранее.
//
// Передаёт право только сам согласующий. Возможность назначить делегата за
// другого человека означала бы, что администратор собирает себе кворум из
// делегатов, а это ровно то, что согласование должно предотвращать.
type ApprovalDelegation struct {
	ID string `json:"id"`
	// Delegator — чьё право голоса передано.
	Delegator string `json:"delegator"`
	// Delegate — кому. Обязателен и должен быть заведённой учётной записью:
	// делегат сначала входит под собой, и только потом предъявляет токен. В
	// журнале видно обоих, а «кто угодно с токеном» — это ровно та передача
	// пароля по переписке, от которой всё и затевалось.
	Delegate string `json:"delegate"`
	// GroupName ограничивает делегирование одной группой. Пусто — все группы,
	// в которых состоит делегирующий на момент голосования.
	GroupName string `json:"group_name,omitempty"`
	// Reason виден делегату и в журнале: «отпуск до 12-го» объясняет чужой
	// голос лучше, чем его отсутствие.
	Reason string `json:"reason,omitempty"`
	// Prefix — открытая часть токена, по ней идёт поиск.
	Prefix string `json:"prefix"`
	// TokenHash и PasswordHash хранятся раздельно и считаются по-разному:
	// в токене 256 бит из crypto/rand и хватает SHA-256, пароль придумывает
	// человек и требует медленного хеша.
	TokenHash    []byte `json:"-"`
	PasswordHash []byte `json:"-"`

	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	// UsedCount и LastUsedAt нужны владельцу: делегирование, которым
	// воспользовались чаще ожидаемого, — повод его отозвать.
	UsedCount  int        `json:"used_count"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// MaxDelegationTTL — предел срока делегирования.
//
// Бессрочная передача права голоса — это не делегирование, а вторая учётная
// запись у того же человека. Тридцати суток хватает на отпуск; на декрет
// нужно менять состав группы, а не продлевать токен.
const MaxDelegationTTL = 30 * 24 * time.Hour

// Usable сообщает, годится ли делегирование прямо сейчас.
func (d ApprovalDelegation) Usable(now time.Time) bool {
	return d.RevokedAt == nil && now.Before(d.ExpiresAt)
}

// Covers сообщает, распространяется ли делегирование на эту группу.
func (d ApprovalDelegation) Covers(group string) bool {
	return d.GroupName == "" || d.GroupName == group
}
