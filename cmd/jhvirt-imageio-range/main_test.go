package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/imageio"
)

func TestReadTicketRequiresDirectHTTPS(t *testing.T) {
	const endpoint = "https://imageio.example.test:54322/images/5756ecc7-4e13-4e71-9489-d7e642fa2c6e"
	if _, err := readTicket(strings.NewReader(endpoint + "\n")); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{endpoint, endpoint + "?proxy=true\n", strings.Replace(endpoint, "https:", "http:", 1) + "\n"} {
		if _, err := readTicket(strings.NewReader(invalid)); err == nil {
			t.Fatal("accepted an invalid ticket input")
		}
	}
}

func TestTransferUsesImageIORanges(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPut {
			body, err := io.ReadAll(request.Body)
			if err != nil || string(body) != "cd" || request.ContentLength != 2 ||
				request.Header.Get("Content-Range") != "bytes 2-3/*" || request.URL.RawQuery != "" {
				t.Error("PUT did not send only the requested source section with flush enabled")
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		if request.Header.Get("Range") != "bytes=2-3" {
			t.Error("wrong GET range")
		}
		writer.Header().Set("Content-Range", "bytes 2-3/6")
		writer.Header().Set("Content-Length", "2")
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(writer, "cd")
	}))
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	client := imageio.New(server.URL, dataClient(roots, 2, 2))
	file := filepath.Join(t.TempDir(), "test-image")
	if err := os.WriteFile(file, []byte("abcdef"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := transfer(context.Background(), client, "upload", file, 2, 2, io.Discard); err != nil {
		t.Fatal(err)
	}
	var downloaded bytes.Buffer
	if err := transfer(context.Background(), client, "download", "", 2, 2, &downloaded); err != nil || downloaded.String() != "cd" {
		t.Fatalf("downloaded %q, error %v", downloaded.String(), err)
	}
	if err := transfer(context.Background(), client, "upload", file, 5, 2, io.Discard); err == nil {
		t.Fatal("accepted a range past the source file")
	}
}

func TestTransferRejectsWrongContentRange(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Range", "bytes 0-1/6")
		writer.Header().Set("Content-Length", "2")
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(writer, "ab")
	}))
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	client := imageio.New(server.URL, dataClient(roots, 2, 2))
	var downloaded bytes.Buffer
	if err := transfer(context.Background(), client, "download", "", 2, 2, &downloaded); err == nil || downloaded.Len() != 0 {
		t.Fatal("accepted a response for a different offset")
	}
}
