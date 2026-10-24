package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDownloadFileUsesServerModificationTime(t *testing.T) {
	serverTime := time.Date(2025, time.March, 4, 5, 6, 7, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("If-Modified-Since"); got == "" {
			t.Error("request did not include If-Modified-Since")
		}
		writer.Header().Set("Last-Modified", serverTime.Format(http.TimeFormat))
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("new content"))
	}))
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "nested", "download.txt")
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatalf("create destination directory: %v", err)
	}
	if err := os.WriteFile(destination, []byte("old content"), 0o644); err != nil {
		t.Fatalf("create old local file: %v", err)
	}
	if err := os.Chtimes(destination, serverTime.Add(-time.Minute), serverTime.Add(-time.Minute)); err != nil {
		t.Fatalf("set old local modification time: %v", err)
	}

	updated, err := downloadFile(context.Background(), server.Client(), Download{
		URL:         server.URL,
		Destination: destination,
	})
	if err != nil {
		t.Fatalf("downloadFile returned an error: %v", err)
	}
	if !updated {
		t.Fatal("downloadFile reported no update")
	}

	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if string(data) != "new content" {
		t.Fatalf("downloaded content = %q, want %q", data, "new content")
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatalf("stat downloaded file: %v", err)
	}
	if !info.ModTime().Equal(serverTime) {
		t.Fatalf("modification time = %s, want server time %s", info.ModTime(), serverTime)
	}
}

func TestDownloadFileSkipsOlderServerVersion(t *testing.T) {
	localTime := time.Date(2025, time.March, 4, 5, 6, 8, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("If-Modified-Since"); got == "" {
			t.Error("request did not include If-Modified-Since")
		}
		writer.Header().Set("Last-Modified", localTime.Add(-time.Second).Format(http.TimeFormat))
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("older content"))
	}))
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "download.txt")
	if err := os.WriteFile(destination, []byte("current content"), 0o644); err != nil {
		t.Fatalf("create local file: %v", err)
	}
	if err := os.Chtimes(destination, localTime, localTime); err != nil {
		t.Fatalf("set local modification time: %v", err)
	}

	updated, err := downloadFile(context.Background(), server.Client(), Download{
		URL:         server.URL,
		Destination: destination,
	})
	if err != nil {
		t.Fatalf("downloadFile returned an error: %v", err)
	}
	if updated {
		t.Fatal("downloadFile replaced a newer local file")
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read local file: %v", err)
	}
	if string(data) != "current content" {
		t.Fatalf("local content = %q, want %q", data, "current content")
	}
}

func TestDownloadFileHandlesNotModified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization header = %q, want %q", got, "Bearer test-token")
		}
		if got := request.Header.Get("X-API-Key"); got != "test-key" {
			t.Errorf("X-API-Key header = %q, want %q", got, "test-key")
		}
		writer.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "download.txt")
	if err := os.WriteFile(destination, []byte("unchanged content"), 0o644); err != nil {
		t.Fatalf("create local file: %v", err)
	}

	updated, err := downloadFile(context.Background(), server.Client(), Download{
		URL:         server.URL,
		Destination: destination,
		Headers: map[string]string{
			"Authorization": "Bearer test-token",
			"X-API-Key":     "test-key",
		},
	})
	if err != nil {
		t.Fatalf("downloadFile returned an error: %v", err)
	}
	if updated {
		t.Fatal("downloadFile reported an update for a 304 response")
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read local file: %v", err)
	}
	if string(data) != "unchanged content" {
		t.Fatalf("local content = %q, want %q", data, "unchanged content")
	}
}

func TestNotifyDownloadResult(t *testing.T) {
	tests := []struct {
		name         string
		updated      bool
		downloadErr  error
		wantNotified bool
		wantMessage  string
	}{
		{
			name:         "changed file",
			updated:      true,
			wantNotified: true,
			wantMessage:  "File updated: downloads/archive.zip",
		},
		{
			name:         "download error",
			downloadErr:  errors.New("server unavailable"),
			wantNotified: true,
			wantMessage:  "Download failed: downloads/archive.zip",
		},
		{
			name: "unchanged file",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			gotMessage := ""
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				called = true
				if request.Method != http.MethodPost {
					t.Errorf("request method = %q, want POST", request.Method)
				}
				data, err := io.ReadAll(request.Body)
				if err != nil {
					t.Errorf("read notification body: %v", err)
				}
				gotMessage = string(data)
				writer.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			err := notifyDownloadResult(context.Background(), server.Client(), server.URL, Download{
				Destination: "downloads/archive.zip",
			}, test.updated, test.downloadErr)
			if err != nil {
				t.Fatalf("notifyDownloadResult returned an error: %v", err)
			}
			if called != test.wantNotified {
				t.Fatalf("notification sent = %t, want %t", called, test.wantNotified)
			}
			if gotMessage != test.wantMessage {
				t.Fatalf("notification message = %q, want %q", gotMessage, test.wantMessage)
			}
		})
	}
}

func TestRunSuccessCommand(t *testing.T) {
	if marker := os.Getenv("URLSYNC_TEST_COMMAND_MARKER"); marker != "" {
		if err := os.WriteFile(marker, []byte("ran"), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}

	marker := filepath.Join(t.TempDir(), "command-ran")
	t.Setenv("URLSYNC_TEST_COMMAND_MARKER", marker)
	command := &Command{
		Command: os.Args[0],
		Args:    []string{"-test.run=^TestRunSuccessCommand$"},
	}
	if err := runSuccessCommand(context.Background(), command, true); err != nil {
		t.Fatalf("runSuccessCommand returned an error: %v", err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "ran" {
		t.Fatalf("success command marker = %q, error = %v", data, err)
	}

	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove command marker: %v", err)
	}
	if err := runSuccessCommand(context.Background(), command, false); err != nil {
		t.Fatalf("runSuccessCommand returned an error for an unchanged run: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("command ran for an unchanged result, stat error = %v", err)
	}
}

func TestWriteServiceUnits(t *testing.T) {
	unitDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "settings with spaces.json")
	if err := writeServiceUnits(unitDir, "/opt/urlsync/urlsync", configPath, 90*time.Second); err != nil {
		t.Fatalf("writeServiceUnits returned an error: %v", err)
	}

	service, err := os.ReadFile(filepath.Join(unitDir, "urlsync.service"))
	if err != nil {
		t.Fatalf("read service unit: %v", err)
	}
	for _, expected := range []string{
		"Type=oneshot",
		"ExecStart=\"/opt/urlsync/urlsync\" \"-config\" \"" + configPath + "\"",
		"WorkingDirectory=\"" + filepath.Dir(configPath) + "\"",
	} {
		if !strings.Contains(string(service), expected) {
			t.Errorf("service unit does not contain %q", expected)
		}
	}

	timer, err := os.ReadFile(filepath.Join(unitDir, "urlsync.timer"))
	if err != nil {
		t.Fatalf("read timer unit: %v", err)
	}
	for _, expected := range []string{"OnBootSec=1min", "OnUnitActiveSec=1min 30s", "Persistent=true", "Unit=urlsync.service"} {
		if !strings.Contains(string(timer), expected) {
			t.Errorf("timer unit does not contain %q", expected)
		}
	}
}
