package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConsoleCancelsBlockedPrompt(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	ui := console{ctx: ctx, input: bufio.NewReader(waitingReader{reader, entered}), output: io.Discard}
	result := make(chan error, 1)
	go func() {
		_, err := ui.ask("Ввод")
		result <- err
	}()
	select {
	case <-entered:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("prompt did not enter its read")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("prompt waits for terminal input after cancellation")
	}
}

type waitingReader struct {
	io.Reader
	entered chan struct{}
}

func (reader waitingReader) Read(data []byte) (int, error) {
	close(reader.entered)
	return reader.Reader.Read(data)
}

func TestSavedDeliveryRecordsPreparedImage(t *testing.T) {
	job := t.TempDir()
	image := filepath.Join(job, testDisk+".raw")
	if err := os.WriteFile(image, []byte("prepared image"), 0600); err != nil {
		t.Fatal(err)
	}
	destination, err := destinationFromInput("ops@hv.example.test", "/data/transfer")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveDelivery(job, []string{image}, nil, destination); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.Open(filepath.Join(job, deliveryManifest))
	if err != nil {
		t.Fatal(err)
	}
	defer manifest.Close()
	task, err := readDelivery(manifest)
	if err != nil || len(task.Files) != 1 || task.Host != destination.host {
		t.Fatalf("could not read back delivery: %+v, %v", task, err)
	}
	if err := os.WriteFile(image, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := task.Files[0].unchanged(job); err == nil || !strings.Contains(err.Error(), "изменился") {
		t.Fatalf("changed image was accepted: %v", err)
	}
}

func TestRemovePreparedImagesKeepsUnrelatedAndChangedFiles(t *testing.T) {
	job := t.TempDir()
	root, err := os.OpenRoot(job)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, name := range []string{testDisk + ".raw", testBase + ".qcow2", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(job, name), []byte("initial"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := inspectDeliveryFile(job, testDisk+".raw")
	if err != nil {
		t.Fatal(err)
	}
	qcow, err := inspectDeliveryFile(job, testBase+".qcow2")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(job, qcow.Name), []byte("replaced file"), 0600); err != nil {
		t.Fatal(err)
	}
	files := []deliveryFile{raw, qcow}
	for _, want := range []int{1, 0} {
		removed, err := removePreparedImages(t.Context(), root, files)
		if removed != want || err == nil || !strings.Contains(err.Error(), qcow.Name) {
			t.Fatalf("removed %d, expected %d and a changed file error, got %v", removed, want, err)
		}
	}
	for _, name := range []string{qcow.Name, "notes.txt"} {
		if _, err := root.Stat(name); err != nil {
			t.Fatalf("%q was removed: %v", name, err)
		}
	}
}
