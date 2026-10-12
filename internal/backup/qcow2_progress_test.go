package backup

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// qemu-img -p пишет долю выполненного кусками, и граница записи может прийтись
// на середину «(12.50/100%)».
func TestQemuProgressParsesSplitOutput(t *testing.T) {
	var got []float64
	p := &qemuProgress{report: func(v float64) { got = append(got, v) }}
	for _, part := range []string{"    (0.00/100%)\r", "    (12.5", "0/100%)\r", "    (100.00/100%)\r\n"} {
		if n, err := p.Write([]byte(part)); err != nil || n != len(part) {
			t.Fatalf("Write(%q) = %d, %v", part, n, err)
		}
	}
	if len(got) == 0 || got[0] != 0 || got[len(got)-1] != 1 {
		t.Fatalf("доли: %v — ждали от 0 до 1", got)
	}
	half := false
	for i, v := range got {
		if v == 0.125 {
			half = true
		}
		if i > 0 && v < got[i-1] {
			t.Fatalf("доля пошла назад: %v", got)
		}
	}
	if !half {
		t.Fatalf("доля 12.50 %%, разорванная между записями, не разобрана: %v", got)
	}
}

// Без получателя вывод просто поглощается.
func TestQemuProgressWithoutReceiver(t *testing.T) {
	p := &qemuProgress{}
	out := []byte("    (50.00/100%)\r")
	if n, err := p.Write(out); err != nil || n != len(out) {
		t.Fatalf("Write = %d, %v", n, err)
	}
}

// fakeQemuImg ставит вместо qemu-img сценарий: он печатает ход, как qemu-img
// -p, а при fail пишет ошибку в stderr и выходит с кодом 1.
func fakeQemuImg(t *testing.T, fail bool) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		body := "@echo off\r\necho     (0.00/100%%)\r\necho     (50.00/100%%)\r\necho     (100.00/100%%)\r\n"
		if fail {
			body = "@echo off\r\necho     (10.00/100%%)\r\necho no space left 1>&2\r\nexit /b 1\r\n"
		}
		path := filepath.Join(dir, "qemu-img.bat")
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	body := "#!/bin/sh\nprintf '    (0.00/100%%)\\r    (50.00/100%%)\\r    (100.00/100%%)\\n'\n"
	if fail {
		body = "#!/bin/sh\nprintf '    (10.00/100%%)\\r'\necho 'no space left' >&2\nexit 1\n"
	}
	path := filepath.Join(dir, "qemu-img")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Сборка образа сообщает ход по выводу qemu-img и не теряет его ошибку.
func TestConvertQcow2ToRawReportsProgress(t *testing.T) {
	var got []float64
	err := ConvertQcow2ToRaw(context.Background(), fakeQemuImg(t, false), "top.qcow2", "image.raw",
		func(v float64) { got = append(got, v) })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[len(got)-1] != 1 {
		t.Fatalf("ход сборки: %v — ждали до 1", got)
	}
	// Без получателя хода команда работает так же.
	if err := ConvertQcow2ToRaw(context.Background(), fakeQemuImg(t, false), "top.qcow2", "image.raw", nil); err != nil {
		t.Fatal(err)
	}
}

func TestConvertQcow2ToRawKeepsQemuError(t *testing.T) {
	err := ConvertQcow2ToRaw(context.Background(), fakeQemuImg(t, true), "top.qcow2", "image.raw", func(float64) {})
	if err == nil || !strings.Contains(err.Error(), "no space left") {
		t.Fatalf("ждали ошибку qemu-img с текстом из stderr: %v", err)
	}
	if strings.Contains(err.Error(), "/100%") {
		t.Fatalf("в ошибку попал вывод хода: %v", err)
	}
}
