package backup

import (
	"reflect"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

func checkpointAt(id, parent string, minute int) ovirt.Checkpoint {
	return ovirt.Checkpoint{ID: id, ParentID: parent,
		CreationDate: ovirt.Timestamp{When: time.Date(2026, 9, 25, 10, minute, 0, 0, time.UTC)}}
}

// Движок удаляет только корень, поэтому порядок берётся из ссылок на
// родителя, а не из порядка в ответе API.
func TestOvirtCheckpointChainFollowsParents(t *testing.T) {
	got := ovirtCheckpointChain([]ovirt.Checkpoint{
		checkpointAt("c3", "c2", 3),
		checkpointAt("c1", "", 1),
		checkpointAt("c2", "c1", 2),
	})
	if want := []string{"c1", "c2", "c3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("цепочка %v, ожидалось %v", got, want)
	}
}

// Корень, чей родитель уже удалён, всё равно корень.
func TestOvirtCheckpointChainRootWithMissingParent(t *testing.T) {
	got := ovirtCheckpointChain([]ovirt.Checkpoint{
		checkpointAt("c5", "c4", 5),
		checkpointAt("c4", "c3-deleted", 4),
	})
	if want := []string{"c4", "c5"}; !reflect.DeepEqual(got, want) {
		t.Errorf("цепочка %v, ожидалось %v", got, want)
	}
}

// Ссылки не складываются в одну цепочку — порядок по времени создания.
func TestOvirtCheckpointChainFallsBackToCreationTime(t *testing.T) {
	got := ovirtCheckpointChain([]ovirt.Checkpoint{
		checkpointAt("b", "a", 2),
		checkpointAt("c", "a", 3),
		checkpointAt("a", "", 1),
	})
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("цепочка %v, ожидалось %v", got, want)
	}
}
