package backup

import (
	"reflect"
	"strings"
	"testing"
)

func ownByPrefix(id string) bool { return strings.HasPrefix(id, "jhv-") }

func TestCheckpointsToDrop(t *testing.T) {
	for _, tc := range []struct {
		name     string
		chain    []string
		keep     map[string]bool
		rootOnly bool
		want     []string
	}{
		{
			name:  "libvirt: удаляются все свои, кроме нужных",
			chain: []string{"jhv-1", "jhv-2", "jhv-3", "jhv-4", "jhv-5"},
			keep:  map[string]bool{"jhv-2": true, "jhv-5": true},
			want:  []string{"jhv-1", "jhv-3", "jhv-4"},
		},
		{
			name:  "чужие checkpoint-ы не трогаются",
			chain: []string{"vendor-a", "jhv-1", "vendor-b", "jhv-2", "jhv-3"},
			keep:  map[string]bool{"jhv-3": true},
			want:  []string{"jhv-1", "jhv-2"},
		},
		{
			name:     "oVirt: только с корня, до первого, который трогать нельзя",
			chain:    []string{"jhv-1", "jhv-2", "jhv-3", "jhv-4", "jhv-5"},
			keep:     map[string]bool{"jhv-3": true, "jhv-5": true},
			rootOnly: true,
			want:     []string{"jhv-1", "jhv-2"},
		},
		{
			name:     "oVirt: чужой корень останавливает ротацию",
			chain:    []string{"vendor-a", "jhv-1", "jhv-2"},
			keep:     map[string]bool{"jhv-2": true},
			rootOnly: true,
			want:     nil,
		},
		{
			name:  "самый новый не удаляется, даже если каталог о нём ещё не знает",
			chain: []string{"jhv-1", "jhv-2", "jhv-3"},
			keep:  map[string]bool{"jhv-1": true},
			want:  []string{"jhv-2"},
		},
		{
			name:  "без сведений о нужных не удаляется ничего",
			chain: []string{"jhv-1", "jhv-2", "jhv-3"},
			keep:  nil,
			want:  nil,
		},
		{
			name:  "единственный checkpoint остаётся",
			chain: []string{"jhv-1"},
			keep:  map[string]bool{"jhv-0": true},
			want:  nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckpointsToDrop(tc.chain, ownByPrefix, tc.keep, tc.rootOnly)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("CheckpointsToDrop = %v, ожидалось %v", got, tc.want)
			}
		})
	}
}
