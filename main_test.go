package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
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
			wantMessage:  "[urlsync] File updated: downloads/archive.zip",
		},
		{
			name:         "download error",
			downloadErr:  errors.New("server unavailable"),
			wantNotified: true,
			wantMessage:  "[urlsync] Download failed: downloads/archive.zip",
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

			err := notifyDownloadResult(context.Background(), server.Client(), server.URL, "[urlsync] ", Download{
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
	if err := writeServiceUnits(unitDir, "/opt/urlsync/urlsync", configPath, "urlsync", 90*time.Second, "", 0, true); err != nil {
		t.Fatalf("writeServiceUnits returned an error: %v", err)
	}

	service, err := os.ReadFile(filepath.Join(unitDir, "urlsync.service"))
	if err != nil {
		t.Fatalf("read service unit: %v", err)
	}
	for _, expected := range []string{
		"Requires=network-online.target",
		"After=network-online.target",
		"Type=oneshot",
		"ExecStart=\"/opt/urlsync/urlsync\" \"-config\" \"" + configPath + "\"",
		"WorkingDirectory=" + systemdPathEscape(filepath.Dir(configPath)),
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

func TestWriteServiceUnitsMakesWorkingDirectoryAbsolute(t *testing.T) {
	unitDir := t.TempDir()
	configPath := filepath.Join("relative", "settings.json")
	if err := writeServiceUnits(unitDir, "/opt/urlsync/urlsync", configPath, "urlsync", time.Minute, "", 0, false); err != nil {
		t.Fatalf("writeServiceUnits returned an error: %v", err)
	}

	service, err := os.ReadFile(filepath.Join(unitDir, "urlsync.service"))
	if err != nil {
		t.Fatalf("read service unit: %v", err)
	}
	workingDirectory, err := filepath.Abs(filepath.Dir(configPath))
	if err != nil {
		t.Fatalf("resolve expected working directory: %v", err)
	}
	expected := "WorkingDirectory=" + systemdPathEscape(workingDirectory)
	if !strings.Contains(string(service), expected) {
		t.Errorf("service unit does not contain absolute working directory %q", expected)
	}
	if strings.Contains(string(service), "network-online.target") {
		t.Error("user service unexpectedly depends on the system network-online target")
	}
}

func TestSystemdPathEscape(t *testing.T) {
	for _, test := range []struct {
		path string
		want string
	}{
		{"/tmp/path with spaces", `/tmp/path\x20with\x20spaces`},
		{`/tmp/path "quoted"`, `/tmp/path\x20\x22quoted\x22`},
		{`/tmp/back\slash`, `/tmp/back\x5cslash`},
		{"/tmp/100%", `/tmp/100%%`},
	} {
		if got := systemdPathEscape(test.path); got != test.want {
			t.Errorf("systemdPathEscape(%q) = %q, want %q", test.path, got, test.want)
		}
	}
}

func TestWriteServiceUnitsEscapesWorkingDirectory(t *testing.T) {
	unitDir := t.TempDir()
	configDir := filepath.Join(t.TempDir(), `directory "with spaces"`)
	if err := os.Mkdir(configDir, 0o755); err != nil {
		t.Fatalf("create config directory: %v", err)
	}
	configPath := filepath.Join(configDir, "settings.json")
	if err := writeServiceUnits(unitDir, "/opt/urlsync/urlsync", configPath, "urlsync", time.Minute, "", 0, false); err != nil {
		t.Fatalf("writeServiceUnits returned an error: %v", err)
	}
	service, err := os.ReadFile(filepath.Join(unitDir, "urlsync.service"))
	if err != nil {
		t.Fatalf("read service unit: %v", err)
	}
	directive := "WorkingDirectory=" + systemdPathEscape(configDir)
	if !strings.Contains(string(service), directive) {
		t.Errorf("service unit does not contain escaped working directory %q", directive)
	}
	for _, line := range strings.Split(string(service), "\n") {
		if strings.HasPrefix(line, "WorkingDirectory=") && strings.ContainsAny(line, `"'`) {
			t.Errorf("WorkingDirectory contains literal quote characters: %q", line)
		}
	}
}

func TestParseSystemdDuration(t *testing.T) {
	for _, test := range []struct {
		value string
		want  time.Duration
	}{
		{"2d", 48 * time.Hour},
		{"2d12h30m", 60*time.Hour + 30*time.Minute},
		{"1d2h", 26 * time.Hour},
		{"15m", 15 * time.Minute},
	} {
		got, err := parseSystemdDuration(test.value)
		if err != nil {
			t.Errorf("parseSystemdDuration(%q) returned an error: %v", test.value, err)
		} else if got != test.want {
			t.Errorf("parseSystemdDuration(%q) = %v, want %v", test.value, got, test.want)
		}
	}
}

func TestWriteServiceUnitsCalendarAndRandomizedDelay(t *testing.T) {
	unitDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "settings.json")
	calendar := "Mon *-*-* 10:00:00"
	if err := writeServiceUnits(unitDir, "/opt/urlsync/urlsync", configPath, "urlsync", 0, calendar, 90*time.Second, false); err != nil {
		t.Fatalf("writeServiceUnits returned an error: %v", err)
	}

	timer, err := os.ReadFile(filepath.Join(unitDir, "urlsync.timer"))
	if err != nil {
		t.Fatalf("read timer unit: %v", err)
	}
	for _, expected := range []string{"OnCalendar=" + calendar, "RandomizedDelaySec=1min 30s"} {
		if !strings.Contains(string(timer), expected) {
			t.Errorf("timer unit does not contain %q", expected)
		}
	}
	for _, unexpected := range []string{"OnBootSec=", "OnUnitActiveSec="} {
		if strings.Contains(string(timer), unexpected) {
			t.Errorf("calendar timer unexpectedly contains %q", unexpected)
		}
	}
}

func TestWriteServiceUnitsUsesCustomName(t *testing.T) {
	unitDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "settings.json")
	if err := writeServiceUnits(unitDir, "/opt/urlsync/urlsync", configPath, "nightly-sync", time.Hour, "", 0, false); err != nil {
		t.Fatalf("writeServiceUnits returned an error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(unitDir, "nightly-sync.service")); err != nil {
		t.Fatalf("custom service file not found: %v", err)
	}
	timer, err := os.ReadFile(filepath.Join(unitDir, "nightly-sync.timer"))
	if err != nil {
		t.Fatalf("read custom timer unit: %v", err)
	}
	if !strings.Contains(string(timer), "Unit=nightly-sync.service") {
		t.Error("custom timer does not point to custom service")
	}
}

func TestWriteServiceUnitsDoesNotOverwriteExistingUnits(t *testing.T) {
	for _, existingUnit := range []string{"backup.service", "backup.timer"} {
		t.Run(existingUnit, func(t *testing.T) {
			unitDir := t.TempDir()
			configPath := filepath.Join(t.TempDir(), "settings.json")
			existingPath := filepath.Join(unitDir, existingUnit)
			existingContent := []byte("existing unit contents\n")
			if err := os.WriteFile(existingPath, existingContent, 0o644); err != nil {
				t.Fatalf("create existing unit: %v", err)
			}

			err := writeServiceUnits(unitDir, "/opt/urlsync/urlsync", configPath, "backup", time.Hour, "", 0, false)
			if err == nil {
				t.Fatal("writeServiceUnits succeeded despite an existing unit")
			}
			if content, readErr := os.ReadFile(existingPath); readErr != nil {
				t.Fatalf("read existing unit: %v", readErr)
			} else if string(content) != string(existingContent) {
				t.Errorf("existing unit was changed: got %q", content)
			}
			if existingUnit == "backup.timer" {
				if _, statErr := os.Stat(filepath.Join(unitDir, "backup.service")); !os.IsNotExist(statErr) {
					t.Errorf("new service unit was not rolled back, stat error = %v", statErr)
				}
			}
		})
	}
}

func TestRemoveServiceUnits(t *testing.T) {
	unitDir := t.TempDir()
	for _, filename := range []string{"backup.service", "backup.timer", "other.service"} {
		if err := os.WriteFile(filepath.Join(unitDir, filename), []byte(filename), 0o644); err != nil {
			t.Fatalf("create %s: %v", filename, err)
		}
	}

	if err := removeServiceUnits(unitDir, "backup"); err != nil {
		t.Fatalf("removeServiceUnits returned an error: %v", err)
	}
	for _, filename := range []string{"backup.service", "backup.timer"} {
		if _, err := os.Stat(filepath.Join(unitDir, filename)); !os.IsNotExist(err) {
			t.Errorf("%s was not removed, stat error = %v", filename, err)
		}
	}
	if content, err := os.ReadFile(filepath.Join(unitDir, "other.service")); err != nil || string(content) != "other.service" {
		t.Errorf("unrelated unit was changed: content %q, error %v", content, err)
	}
}

func TestRemoveServiceUnitsReturnsErrorWhenNothingExists(t *testing.T) {
	if err := removeServiceUnits(t.TempDir(), "missing"); err == nil {
		t.Fatal("removeServiceUnits succeeded when no units existed")
	}
}

func TestValidSystemdUnitName(t *testing.T) {
	for _, test := range []struct {
		name  string
		valid bool
	}{
		{"urlsync", true},
		{"nightly-sync_1", true},
		{"", false},
		{"../urlsync", false},
		{".hidden", false},
		{"urlsync.service", false},
		{"urlsync.timer", false},
	} {
		if got := validSystemdUnitName(test.name); got != test.valid {
			t.Errorf("validSystemdUnitName(%q) = %v, want %v", test.name, got, test.valid)
		}
	}
}

func TestSystemdDurationIncludesDays(t *testing.T) {
	if got, want := systemdDuration(49*time.Hour+30*time.Minute), "2d 1h 30min"; got != want {
		t.Errorf("systemdDuration() = %q, want %q", got, want)
	}
}

func TestRootUsageIncludesServiceOptions(t *testing.T) {
	var output bytes.Buffer
	writeRootUsage(&output, "urlsync")
	for _, expected := range []string{
		"install-service [options]",
		"uninstall-service [options]",
		"-config",
		"-example-config",
		"-name",
		"-interval",
		"-on-calendar",
		"-randomized-delay",
		"-system",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("root help does not contain %q", expected)
		}
	}
}

func TestCreateExampleConfig(t *testing.T) {
	directory := t.TempDir()
	if err := createExampleConfig(directory); err != nil {
		t.Fatalf("createExampleConfig returned an error: %v", err)
	}

	path := filepath.Join(directory, "settings.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("example settings.json was not created: %v", err)
	}
	if _, err := loadConfig(path); err != nil {
		t.Fatalf("example settings.json is invalid: %v", err)
	}
}

func TestCreateExampleConfigDoesNotOverwrite(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "settings.json")
	existingContent := []byte("keep this config\n")
	if err := os.WriteFile(path, existingContent, 0o600); err != nil {
		t.Fatalf("create existing config: %v", err)
	}

	if err := createExampleConfig(directory); err == nil {
		t.Fatal("createExampleConfig succeeded despite an existing config")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read existing config: %v", err)
	}
	if string(content) != string(existingContent) {
		t.Errorf("existing config was overwritten: got %q", content)
	}
}

func TestServiceCommandsAcceptHelp(t *testing.T) {
	if err := installServiceCommand([]string{"-help"}); err != nil {
		t.Errorf("installServiceCommand -help returned an error: %v", err)
	}
	if err := uninstallServiceCommand([]string{"-help"}); err != nil {
		t.Errorf("uninstallServiceCommand -help returned an error: %v", err)
	}
}

func TestWindowsTaskSchedule(t *testing.T) {
	now := time.Date(2026, time.October, 24, 21, 7, 0, 0, time.Local)
	for _, test := range []struct {
		name              string
		interval          time.Duration
		calendar          string
		randomizedDelay   time.Duration
		wantBoundary      string
		wantSchedule      string
		wantRepetition    string
		wantErrorContains string
	}{
		{
			name:           "interval minutes",
			interval:       90 * time.Minute,
			wantBoundary:   "2026-10-24T21:08:00",
			wantSchedule:   "<ScheduleByDay><DaysInterval>1</DaysInterval></ScheduleByDay>",
			wantRepetition: "<Interval>PT90M</Interval>",
		},
		{
			name:         "interval days",
			interval:     48 * time.Hour,
			wantBoundary: "2026-10-24T21:08:00",
			wantSchedule: "<ScheduleByDay><DaysInterval>2</DaysInterval></ScheduleByDay>",
		},
		{
			name:         "weekdays",
			calendar:     "Mon,Tue,Wed,Thu,Fri *-*-* 10:30",
			wantBoundary: "2026-10-24T10:30:00",
			wantSchedule: "<ScheduleByWeek><WeeksInterval>1</WeeksInterval><DaysOfWeek><Monday/><Tuesday/><Wednesday/><Thursday/><Friday/></DaysOfWeek></ScheduleByWeek>",
		},
		{
			name:           "hourly alias",
			calendar:       "hourly",
			wantBoundary:   "2026-10-24T00:00:00",
			wantSchedule:   "<ScheduleByDay><DaysInterval>1</DaysInterval></ScheduleByDay>",
			wantRepetition: "<Interval>PT1H</Interval>",
		},
		{
			name:         "weekly alias",
			calendar:     "weekly",
			wantBoundary: "2026-10-24T00:00:00",
			wantSchedule: "<ScheduleByWeek><WeeksInterval>1</WeeksInterval><DaysOfWeek><Monday/></DaysOfWeek></ScheduleByWeek>",
		},
		{
			name:              "unsupported calendar",
			calendar:          "Mon-Fri",
			wantErrorContains: "unsupported Windows calendar expression",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			boundary, schedule, repetition, err := windowsTaskSchedule(test.interval, test.calendar, test.randomizedDelay, now)
			if test.wantErrorContains != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErrorContains) {
					t.Fatalf("windowsTaskSchedule error = %v, want it to contain %q", err, test.wantErrorContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("windowsTaskSchedule returned an error: %v", err)
			}
			if boundary != test.wantBoundary {
				t.Errorf("start boundary = %q, want %q", boundary, test.wantBoundary)
			}
			if schedule != test.wantSchedule {
				t.Errorf("schedule = %q, want %q", schedule, test.wantSchedule)
			}
			if test.wantRepetition != "" && !strings.Contains(repetition, test.wantRepetition) {
				t.Errorf("repetition = %q, want it to contain %q", repetition, test.wantRepetition)
			}
		})
	}
}

func TestWindowsQuoteArgument(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
	}{
		{"plain", "plain"},
		{"with space", `"with space"`},
		{`C:\some path\`, `"C:\some path\\"`},
		{`say "hello"`, `"say \"hello\""`},
	} {
		if got := windowsQuoteArgument(test.input); got != test.want {
			t.Errorf("windowsQuoteArgument(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func TestWindowsTaskXML(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "settings & data.json")
	taskData, err := windowsTaskXML("nightly-sync", `C:\Program Files\urlsync.exe`, configPath, 24*time.Hour, "", 5*time.Minute, time.Date(2026, time.October, 24, 12, 0, 0, 0, time.Local))
	if err != nil {
		t.Fatalf("windowsTaskXML returned an error: %v", err)
	}
	if !bytes.HasPrefix(taskData, []byte{0xff, 0xfe}) {
		t.Fatal("Task Scheduler XML does not have a UTF-16LE BOM")
	}
	encoded := make([]uint16, (len(taskData)-2)/2)
	for i := range encoded {
		encoded[i] = uint16(taskData[2+i*2]) | uint16(taskData[3+i*2])<<8
	}
	document := string(utf16.Decode(encoded))
	document = strings.Replace(document, `encoding="UTF-16"`, `encoding="UTF-8"`, 1)
	var task struct {
		XMLName xml.Name `xml:"Task"`
	}
	if err := xml.Unmarshal([]byte(document), &task); err != nil {
		t.Fatalf("generated task definition is invalid XML: %v", err)
	}
	for _, expected := range []string{
		`<RandomDelay>PT5M</RandomDelay>`,
		`<Command>C:\Program Files\urlsync.exe</Command>`,
		`settings &amp; data.json`,
		`<UserId>`,
		`<LogonType>Password</LogonType>`,
	} {
		if !strings.Contains(document, expected) {
			t.Errorf("task definition does not contain %q", expected)
		}
	}
}
