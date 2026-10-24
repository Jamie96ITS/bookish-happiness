package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Files     []Download `json:"files"`
	NtfyURL   string     `json:"ntfy_url,omitempty"`
	OnSuccess *Command   `json:"on_success,omitempty"`
}

type Download struct {
	URL         string            `json:"url"`
	Destination string            `json:"destination"`
	Headers     map[string]string `json:"headers,omitempty"`
}

type Command struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "install-service" {
		if err := installServiceCommand(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}

	configPath := flag.String("config", "settings.json", "path to the JSON settings file")
	flag.Parse()

	config, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	var failures []error
	anyUpdated := false
	for _, file := range config.Files {
		updated, err := downloadFile(context.Background(), client, file)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", file.URL, err))
			log.Printf("error: %s: %v", file.URL, err)
			if notifyErr := notifyDownloadResult(context.Background(), client, config.NtfyURL, file, false, err); notifyErr != nil {
				log.Printf("ntfy notification failed: %v", notifyErr)
			}
			continue
		}
		if updated {
			anyUpdated = true
			log.Printf("updated %s -> %s", file.URL, file.Destination)
		} else {
			log.Printf("unchanged %s", file.URL)
		}
		if notifyErr := notifyDownloadResult(context.Background(), client, config.NtfyURL, file, updated, nil); notifyErr != nil {
			log.Printf("ntfy notification failed: %v", notifyErr)
		}
	}
	if err := errors.Join(failures...); err != nil {
		log.Fatal(err)
	}
	if err := runSuccessCommand(context.Background(), config.OnSuccess, anyUpdated); err != nil {
		log.Fatal(err)
	}
}

func installServiceCommand(args []string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("install-service is only supported on Linux with systemd")
	}

	flags := flag.NewFlagSet("install-service", flag.ContinueOnError)
	configPath := flags.String("config", "settings.json", "path to the JSON settings file")
	interval := flags.String("interval", "15m", "time between runs (for example 15m or 1h)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("install-service does not accept positional arguments")
	}

	duration, err := time.ParseDuration(*interval)
	if err != nil || duration < time.Second || duration%time.Second != 0 {
		return fmt.Errorf("interval must be a whole number of seconds, at least 1s")
	}
	absoluteConfig, err := filepath.Abs(*configPath)
	if err != nil {
		return fmt.Errorf("resolve config path: %w", err)
	}
	configInfo, err := os.Stat(absoluteConfig)
	if err != nil {
		return fmt.Errorf("inspect config file: %w", err)
	}
	if configInfo.IsDir() {
		return fmt.Errorf("config path is a directory: %s", absoluteConfig)
	}

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("locate user config directory: %w", err)
	}
	unitDir := filepath.Join(configDir, "systemd", "user")
	if err := writeServiceUnits(unitDir, executable, absoluteConfig, duration); err != nil {
		return err
	}
	if err := runSystemctl("--user", "daemon-reload"); err != nil {
		return err
	}
	if err := runSystemctl("--user", "enable", "--now", "urlsync.timer"); err != nil {
		return err
	}
	fmt.Printf("Installed and enabled user timer in %s\n", unitDir)
	return nil
}

func writeServiceUnits(unitDir, executable, configPath string, interval time.Duration) error {
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return fmt.Errorf("create systemd user unit directory: %w", err)
	}
	service := strings.Join([]string{
		"[Unit]",
		"Description=URL Sync download job",
		"",
		"[Service]",
		"Type=oneshot",
		"WorkingDirectory=" + systemdQuote(filepath.Dir(configPath)),
		"ExecStart=" + strings.Join([]string{
			systemdQuote(executable),
			systemdQuote("-config"),
			systemdQuote(configPath),
		}, " "),
		"TimeoutStartSec=infinity",
		"",
	}, "\n")
	timer := strings.Join([]string{
		"[Unit]",
		"Description=Run URL Sync periodically",
		"",
		"[Timer]",
		"OnBootSec=1min",
		"OnUnitActiveSec=" + systemdDuration(interval),
		"Persistent=true",
		"Unit=urlsync.service",
		"",
		"[Install]",
		"WantedBy=timers.target",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(unitDir, "urlsync.service"), []byte(service), 0o644); err != nil {
		return fmt.Errorf("write urlsync.service: %w", err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, "urlsync.timer"), []byte(timer), 0o644); err != nil {
		return fmt.Errorf("write urlsync.timer: %w", err)
	}
	return nil
}

func systemdQuote(value string) string {
	return strings.ReplaceAll(strconv.Quote(value), "%", "%%")
}

func systemdDuration(duration time.Duration) string {
	seconds := int64(duration / time.Second)
	var parts []string
	if hours := seconds / 3600; hours > 0 {
		parts = append(parts, strconv.FormatInt(hours, 10)+"h")
		seconds %= 3600
	}
	if minutes := seconds / 60; minutes > 0 {
		parts = append(parts, strconv.FormatInt(minutes, 10)+"min")
		seconds %= 60
	}
	if seconds > 0 || len(parts) == 0 {
		parts = append(parts, strconv.FormatInt(seconds, 10)+"s")
	}
	return strings.Join(parts, " ")
}

func runSystemctl(args ...string) error {
	command := exec.Command("systemctl", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	var config Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Config{}, fmt.Errorf("parse config: expected a single JSON value")
	}
	if len(config.Files) == 0 {
		return Config{}, fmt.Errorf("parse config: files must contain at least one download")
	}
	if config.NtfyURL != "" {
		parsedURL, err := url.ParseRequestURI(config.NtfyURL)
		if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" {
			return Config{}, fmt.Errorf("parse config: ntfy_url must be an absolute HTTP or HTTPS URL")
		}
	}
	if config.OnSuccess != nil && config.OnSuccess.Command == "" {
		return Config{}, fmt.Errorf("parse config: on_success.command must not be empty")
	}

	for i, file := range config.Files {
		parsedURL, err := url.ParseRequestURI(file.URL)
		if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" {
			return Config{}, fmt.Errorf("parse config: files[%d].url must be an absolute HTTP or HTTPS URL", i)
		}
		if file.Destination == "" {
			return Config{}, fmt.Errorf("parse config: files[%d].destination must not be empty", i)
		}
	}
	return config, nil
}

func runSuccessCommand(ctx context.Context, command *Command, anyUpdated bool) error {
	if command == nil || !anyUpdated {
		return nil
	}

	process := exec.CommandContext(ctx, command.Command, command.Args...)
	process.Stdout = os.Stdout
	process.Stderr = os.Stderr
	if err := process.Run(); err != nil {
		return fmt.Errorf("run on_success command: %w", err)
	}
	return nil
}

func notifyDownloadResult(ctx context.Context, client *http.Client, ntfyURL string, file Download, updated bool, downloadErr error) error {
	if ntfyURL == "" || (!updated && downloadErr == nil) {
		return nil
	}

	message := fmt.Sprintf("File updated: %s", file.Destination)
	if downloadErr != nil {
		message = fmt.Sprintf("Download failed: %s", file.Destination)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, ntfyURL, bytes.NewBufferString(message))
	if err != nil {
		return fmt.Errorf("create ntfy request: %w", err)
	}
	request.Header.Set("Content-Type", "text/plain; charset=utf-8")

	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("send ntfy notification: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("ntfy server returned %s", response.Status)
	}
	return nil
}

func downloadFile(ctx context.Context, client *http.Client, file Download) (bool, error) {
	localInfo, err := os.Stat(file.Destination)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("inspect destination: %w", err)
	}
	hasLocalFile := err == nil

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, file.URL, nil)
	if err != nil {
		return false, fmt.Errorf("create request: %w", err)
	}
	for name, value := range file.Headers {
		request.Header.Set(name, value)
	}
	if hasLocalFile {
		request.Header.Set("If-Modified-Since", localInfo.ModTime().UTC().Format(http.TimeFormat))
	}

	response, err := client.Do(request)
	if err != nil {
		return false, fmt.Errorf("download: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusNotModified {
		return false, nil
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return false, fmt.Errorf("server returned %s", response.Status)
	}

	var serverModTime time.Time
	if value := response.Header.Get("Last-Modified"); value != "" {
		serverModTime, err = http.ParseTime(value)
		if err != nil {
			return false, fmt.Errorf("invalid Last-Modified header %q: %w", value, err)
		}
		if hasLocalFile && !serverModTime.After(localInfo.ModTime()) {
			return false, nil
		}
	}

	destinationDir := filepath.Dir(file.Destination)
	if err := os.MkdirAll(destinationDir, 0o755); err != nil {
		return false, fmt.Errorf("create destination directory: %w", err)
	}
	temporary, err := os.CreateTemp(destinationDir, ".urlsync-*")
	if err != nil {
		return false, fmt.Errorf("create temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if _, err := io.Copy(temporary, response.Body); err != nil {
		temporary.Close()
		return false, fmt.Errorf("write download: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return false, fmt.Errorf("close download: %w", err)
	}
	if !serverModTime.IsZero() {
		if err := os.Chtimes(temporaryPath, time.Now(), serverModTime); err != nil {
			return false, fmt.Errorf("set server modification time: %w", err)
		}
	}
	if err := os.Rename(temporaryPath, file.Destination); err != nil {
		return false, fmt.Errorf("replace destination: %w", err)
	}
	return true, nil
}
