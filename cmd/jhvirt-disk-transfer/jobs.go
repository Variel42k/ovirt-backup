package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var errJobActive = errors.New("это задание уже выполняется другим процессом")

type jobRecord struct {
	Path     string `json:"path"`
	Complete bool   `json:"complete"`
}

func jobCatalog() (string, error) {
	config, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(config, "jhvirt-disk-transfer", "jobs"), nil
}

func rememberJob(job, id string, complete bool) error {
	dir, err := jobCatalog()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if !complete {
		if record, ok := loadJobRecord(filepath.Join(dir, id)); ok && record.Path == job {
			return nil
		}
	}
	return writeJobJSON(dir, id, jobRecord{Path: job, Complete: complete})
}

func loadJobRecord(path string) (jobRecord, bool) {
	var record jobRecord
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16<<10 {
		return record, false
	}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &record) != nil {
		return record, false
	}
	return record, filepath.IsAbs(record.Path) && localJobName.MatchString(filepath.Base(record.Path))
}

type savedJob struct {
	directory string
	date      time.Time
	task      deliveryJob
	tracked   bool
	active    bool
}

func discoverSavedJobs(ctx context.Context) ([]savedJob, error) {
	records := make(map[string]jobRecord)
	paths := make(map[string]bool)
	if catalog, err := jobCatalog(); err == nil {
		entries, _ := os.ReadDir(catalog)
		for _, entry := range entries {
			if !deliveryID.MatchString(entry.Name()) {
				continue
			}
			if record, ok := loadJobRecord(filepath.Join(catalog, entry.Name())); ok {
				records[entry.Name()] = record
				paths[record.Path] = true
			}
		}
	}
	// Older releases wrote only the manifest. Search their default work directory
	// and the top level of mounted local filesystems, without descending into domains.
	roots := []string{"/var/tmp", "/tmp"}
	mounts, _ := hostMounts()
	for _, mount := range mounts {
		if dataMount(mount) && directDiskMount(mount) {
			roots = append(roots, mount.Path)
		}
	}
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, _ := os.ReadDir(root)
		for _, entry := range entries {
			if entry.IsDir() && localJobName.MatchString(entry.Name()) {
				paths[filepath.Join(root, entry.Name())] = true
			}
		}
	}
	var jobs []savedJob
	for directory := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		job, ok := inspectSavedJob(directory)
		if !ok {
			continue
		}
		record := records[job.task.ID]
		job.tracked = record.Path == directory
		if !record.Complete {
			jobs = append(jobs, job)
		}
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].date.Equal(jobs[j].date) {
			return jobs[i].directory < jobs[j].directory
		}
		return jobs[j].date.Before(jobs[i].date)
	})
	return jobs, nil
}

func inspectSavedJob(directory string) (savedJob, bool) {
	canonical, err := stagingDirectory(directory)
	if err != nil || canonical != directory {
		return savedJob{}, false
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return savedJob{}, false
	}
	defer root.Close()
	file, err := openDelivery(root)
	if err != nil {
		return savedJob{}, false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return savedJob{}, false
	}
	task, err := readDelivery(file)
	if err != nil {
		return savedJob{}, false
	}
	// Closing the manifest releases the probe lock. The transfer takes its own
	// exclusive lock before connecting to the recipient.
	active := errors.Is(lockDelivery(file), errJobActive)
	return savedJob{directory: directory, date: info.ModTime(), task: task, active: active}, true
}

func printSavedJobs(ui console, jobs []savedJob) {
	ui.say("SSH-задания, ожидающие подтверждения:")
	for index, job := range jobs {
		var images []string
		for _, image := range job.task.Files {
			label := image.Label
			if label == "" {
				label = image.Name
			}
			images = append(images, label)
		}
		ui.say("  %d. %s · %s:%s · %q", index+1, job.date.Format("02.01.2006 15:04"),
			job.task.Host, job.task.Directory, strings.Join(images, ", "))
		ui.say("     %s", job.directory)
		if job.active {
			ui.say("     Уже выполняется.")
		}
	}
}

func pickSavedJob(ui console, jobs []savedJob, alternative string) (string, error) {
	printSavedJobs(ui, jobs)
	ui.say("  0. %s", alternative)
	for {
		answer, err := ui.ask("Номер задания (Enter — последнее)")
		if err != nil {
			return "", err
		}
		switch answer {
		case "":
			answer = "1"
		case "0":
			return "", nil
		}
		index, err := selectIndexes(answer, len(jobs))
		if err != nil || len(index) != 1 {
			ui.say("Выберите один номер задания.")
			continue
		}
		return jobs[index[0]].directory, nil
	}
}

func chooseSavedJob(ctx context.Context, ui console) (string, error) {
	jobs, err := consoleRead(ctx, func() ([]savedJob, error) { return discoverSavedJobs(ctx) })
	if err != nil {
		return "", err
	}
	if len(jobs) > 0 {
		job, err := pickSavedJob(ui, jobs, "Ввести рабочий каталог вручную")
		if err != nil || job != "" {
			return job, err
		}
	} else {
		ui.say("В доступных каталогах нет заданий, ожидающих подтверждения.")
	}
	return ui.ask("Абсолютный путь к рабочему каталогу на источнике")
}

func chooseStartupJob(ui console, jobs []savedJob) (string, error) {
	for _, job := range jobs {
		if job.active {
			ui.say("Передача уже выполняется: %s", job.directory)
			return "", nil
		}
	}
	switch len(jobs) {
	case 0:
		return "", nil
	case 1:
		if jobs[0].tracked {
			ui.say("Найдено незавершённое задание. Продолжаю %s", jobs[0].directory)
			ui.say("Для нового задания запустите мастер с --new.")
			return jobs[0].directory, nil
		}
	}
	return pickSavedJob(ui, jobs, "Открыть главное меню")
}
