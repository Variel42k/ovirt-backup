package model

import (
	"fmt"
	"regexp"
	"slices"
)

// VMSelector задаёт единые правила отбора ВМ для предпросмотра, планирования и подсчёта
// покрытия.
type VMSelector struct {
	job    *BackupJob
	nameRE *regexp.Regexp
}

// NewVMSelector проверяет и компилирует фильтр задания бэкапа.
func NewVMSelector(job *BackupJob) (*VMSelector, error) {
	if job == nil {
		return nil, fmt.Errorf("не задано задание бэкапа")
	}
	selector := &VMSelector{job: job}
	if job.VMNameRegex != "" {
		re, err := regexp.Compile(job.VMNameRegex)
		if err != nil {
			return nil, fmt.Errorf("некорректное выражение отбора по имени %q: %w", job.VMNameRegex, err)
		}
		selector.nameRE = re
	}
	return selector, nil
}

// Match сообщает о включении и стабильной причине, подходящей для предварительного
// просмотра UI.
// Положительные селекторы ORed; Явные исключения всегда выигрывают.
func (s *VMSelector) Match(vm *VM) (bool, string) {
	if vm == nil {
		return false, "виртуальная машина отсутствует"
	}
	job := s.job
	if slices.Contains(job.ExcludeVMIDs, vm.ID) {
		return false, "исключена явно"
	}
	empty := len(job.VMIDs) == 0 && s.nameRE == nil && len(job.ClusterIDs) == 0 && len(job.Tags) == 0
	if empty {
		return true, "отбор не задан — берутся все ВМ сервера"
	}
	if slices.Contains(job.VMIDs, vm.ID) {
		return true, "выбрана явно"
	}
	if slices.Contains(job.ClusterIDs, vm.ClusterID) {
		return true, "входит в выбранный кластер"
	}
	if s.nameRE != nil && s.nameRE.MatchString(vm.Name) {
		return true, "имя подходит под регулярное выражение"
	}
	for _, tag := range job.Tags {
		if slices.Contains(vm.Tags, tag) {
			return true, "имеет выбранный тег"
		}
	}
	return false, "не подходит под условия отбора"
}
