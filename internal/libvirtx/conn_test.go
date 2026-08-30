package libvirtx

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"testing"

	"golang.org/x/crypto/ssh"
)

const testHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ8xVQ0nMPUNbwvUuPy4LtRsJvVKLxkPHtoT1P9QzUmB"

// Раньше серверу предлагались оба способа входа сразу. Хосту, притворившемуся
// гипервизором, достаточно было отклонить publickey — и клиент сам предъявлял
// пароль. Теперь ключ и пароль это альтернативы.
func TestKeyExcludesPassword(t *testing.T) {
	cfg, err := sshConfig(Config{
		User: "svc", HostKey: testHostKey,
		PrivateKey: generatePrivateKey(t), Password: "пароль",
	})
	if err != nil {
		t.Fatalf("настройка с ключом отвергнута: %v", err)
	}
	if len(cfg.Auth) != 1 {
		t.Fatalf("серверу предлагается %d способов входа, а должен один: ключ", len(cfg.Auth))
	}
}

// Уже заведённые подключения по паролю продолжают работать: обновление службы
// не должно ночью остановить резервное копирование. Новые так завести уже
// нельзя — это проверяется на уровне API.
func TestPasswordAloneStillWorks(t *testing.T) {
	cfg, err := sshConfig(Config{User: "svc", HostKey: testHostKey, Password: "пароль"})
	if err != nil {
		t.Fatalf("вход по паролю отвергнут: %v", err)
	}
	if len(cfg.Auth) != 1 {
		t.Fatalf("способов входа %d, ожидался один", len(cfg.Auth))
	}
}

func TestNoHostKeyNoConnection(t *testing.T) {
	if _, err := sshConfig(Config{User: "svc", Password: "пароль"}); err == nil {
		t.Fatal("подключение без решения по ключу хоста построено")
	}
}

func generatePrivateKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("генерация ключа: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "тест")
	if err != nil {
		t.Fatalf("упаковка ключа: %v", err)
	}
	return string(pem.EncodeToMemory(block))
}
