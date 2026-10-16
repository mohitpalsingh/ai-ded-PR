package modelsvc

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDownloadResumesPartialFile(t *testing.T) {
	payload := bytes.Repeat([]byte("abcdefghij"), 200_000) // 2MB
	var sawRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRange = r.Header.Get("Range")
		http.ServeContent(w, r, "model.gguf", time.Now(), bytes.NewReader(payload))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(dest+".part", payload[:512*1024], 0644); err != nil {
		t.Fatal(err)
	}

	if err := Download(context.Background(), srv.URL, dest, nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatalf(".part should be renamed away, stat err=%v", err)
	}
	if sawRange != "bytes=524288-" {
		t.Fatalf("expected resume Range header, got %q", sawRange)
	}
}

func TestDownloadRestartsWhenServerIgnoresRange(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 256*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(dest+".part", payload[:1000], 0644); err != nil {
		t.Fatal(err)
	}
	if err := Download(context.Background(), srv.URL, dest, nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}
