package model

import "testing"

func TestRestoreVMRequestRejectsControlCharactersInName(t *testing.T) {
	for _, name := range []string{"line\nbreak", "tab\tname", "null\x00name"} {
		req := RestoreVMRequest{RunID: "run", Name: name}
		if err := req.Validate(); err == nil {
			t.Errorf("имя %q с управляющим символом принято", name)
		}
	}
}
