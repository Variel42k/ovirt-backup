package api

import (
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/backup"
)

func backupCompressionOptions() []optionDescriptor {
	descriptions := map[string]string{
		backup.CompressionZstd:   "Баланс скорости и размера; системное значение по умолчанию.",
		backup.CompressionGzip:   "Совместимый gzip-поток для каждого чанка.",
		backup.CompressionS2:     "Сжатие S2 для быстрого чтения и записи; сравните на своих данных.",
		backup.CompressionLZ4:    "Быстрый LZ4; уровни 6-9 включают более затратный поиск совпадений.",
		backup.CompressionBrotli: "Brotli без потерь; высокие уровни требуют больше времени на сжатие.",
		backup.CompressionNone:   "Без сжатия; несжатые чанки по-прежнему проверяются по SHA-256.",
	}
	options := make([]optionDescriptor, 0, len(backup.Compressions))
	for _, name := range backup.Compressions {
		options = append(options, optionDescriptor{Value: name, Title: strings.ToUpper(name), Description: descriptions[name]})
	}
	return options
}
