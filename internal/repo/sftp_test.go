package repo

import (
	"context"
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// testHostKey — настоящий по форме ключ, которого нет ни у одного живого
// сервера: тесты разбирают настройки, а не подключаются.
const testHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ8xVQ0nMPUNbwvUuPy4LtRsJvVKLxkPHtoT1P9QzUmB"

// Раньше пустой ключ хоста означал «подключаться к кому угодно», и заметить это
// было нельзя: перехваченное соединение выглядит как работающее. Теперь такое
// хранилище не открывается вовсе.
func TestSFTPRefusesWithoutHostKeyDecision(t *testing.T) {
	target := &model.StorageTarget{
		Name: "ночные копии", Kind: model.StorageSFTP, Enabled: true,
		Host: "backup.example.org", Username: "svc", Password: "пароль",
	}

	_, err := repoOpen(context.Background(), target)
	if err == nil {
		t.Fatal("хранилище без ключа хоста открылось — подмена сервера осталась бы незамеченной")
	}
	// Имя хранилища в тексте не для красоты: ошибка всплывает в задании, где
	// хранилищ несколько, и «не задан ключ хоста» без имени не подсказывает, какое чинить.
	if !strings.Contains(err.Error(), "ночные копии") {
		t.Errorf("в ошибке нет имени хранилища: %v", err)
	}
}

// Отказ от проверки остаётся возможным — но только как отдельное решение,
// которое видно в интерфейсе и записано в журнал аудита.
func TestSFTPOpensWithExplicitTrust(t *testing.T) {
	target := &model.StorageTarget{
		Name: "лабораторное", Kind: model.StorageSFTP, Enabled: true,
		Host: "backup.example.org", Username: "svc", Password: "пароль",
		TrustAnyHostKey: true,
	}

	if _, err := repoOpen(context.Background(), target); err != nil {
		t.Fatalf("явное разрешение должно приниматься: %v", err)
	}
}
