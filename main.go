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
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Files      []Download `json:"files"`
	NtfyURL    string     `json:"ntfy_url,omitempty"`
	NtfyPrefix string     `json:"ntfy_prefix,omitempty"`
	OnSuccess  *Command   `json:"on_success,omitempty"`
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
	if len(os.Args) > 1 && os.Args[1] == "uninstall-service" {
		if err := uninstallServiceCommand(os.Args[2:]); err != nil {
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
			if notifyErr := notifyDownloadResult(context.Background(), client, config.NtfyURL, config.NtfyPrefix, file, false, err); notifyErr != nil {
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
		if notifyErr := notifyDownloadResult(context.Background(), client, config.NtfyURL, config.NtfyPrefix, file, updated, nil); notifyErr != nil {
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
	name := flags.String("name", "urlsync", "base name for the generated systemd units")
	interval := flags.String("interval", "", "time between runs (for example 15m, 1h, or 2d)")
	onCalendar := flags.String("on-calendar", "", "systemd calendar schedule (for example daily or Mon *-*-* 10:00:00)")
	randomizedDelay := flags.String("randomized-delay", "0s", "maximum randomized timer delay (for example 5m or 1h)")
	system := flags.Bool("system", false, "install a system-wide service (requires root)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("install-service does not accept positional arguments")
	}
	if !validSystemdUnitName(*name) {
		return fmt.Errorf("service name must start with a letter or digit and contain only letters, digits, '.', '_', '@', or '-' (without a .service or .timer suffix)")
	}

	if *interval != "" && *onCalendar != "" {
		return fmt.Errorf("--interval and --on-calendar cannot be used together")
	}
	calendar := strings.TrimSpace(*onCalendar)
	if strings.ContainsAny(calendar, "\r\n") {
		return fmt.Errorf("--on-calendar must be a single-line systemd calendar expression")
	}
	var err error
	var intervalDuration time.Duration
	if calendar == "" {
		intervalValue := *interval
		if intervalValue == "" {
			intervalValue = "15m"
		}
		intervalDuration, err = parseSystemdDuration(intervalValue)
		if err != nil || intervalDuration < time.Second || intervalDuration%time.Second != 0 {
			return fmt.Errorf("interval must be a whole number of seconds, at least 1s (for example 15m or 2d)")
		}
	}
	randomizedDelayDuration, err := parseSystemdDuration(*randomizedDelay)
	if err != nil || randomizedDelayDuration < 0 || randomizedDelayDuration%time.Second != 0 {
		return fmt.Errorf("randomized delay must be a non-negative whole number of seconds")
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
	unitDir := "/etc/systemd/system"
	systemctlArgs := []string{}
	if !*system {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return fmt.Errorf("locate user config directory: %w", err)
		}
		unitDir = filepath.Join(configDir, "systemd", "user")
		systemctlArgs = append(systemctlArgs, "--user")
	}
	if err := writeServiceUnits(unitDir, executable, absoluteConfig, *name, intervalDuration, calendar, randomizedDelayDuration, *system); err != nil {
		return err
	}
	if err := runSystemctl(append(append([]string{}, systemctlArgs...), "daemon-reload")...); err != nil {
		return err
	}
	if err := runSystemctl(append(append([]string{}, systemctlArgs...), "enable", "--now", *name+".timer")...); err != nil {
		return err
	}
	scope := "user"
	if *system {
		scope = "system"
	}
	fmt.Printf("Installed and enabled %s timer in %s\n", scope, unitDir)
	return nil
}

func uninstallServiceCommand(args []string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("uninstall-service is only supported on Linux with systemd")
	}

	flags := flag.NewFlagSet("uninstall-service", flag.ContinueOnError)
	name := flags.String("name", "urlsync", "base name of the systemd units")
	system := flags.Bool("system", false, "uninstall a system-wide service (requires root)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("uninstall-service does not accept positional arguments")
	}
	if !validSystemdUnitName(*name) {
		return fmt.Errorf("service name must start with a letter or digit and contain only letters, digits, '.', '_', '@', or '-' (without a .service or .timer suffix)")
	}

	unitDir := "/etc/systemd/system"
	systemctlArgs := []string{}
	if !*system {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return fmt.Errorf("locate user config directory: %w", err)
		}
		unitDir = filepath.Join(configDir, "systemd", "user")
		systemctlArgs = append(systemctlArgs, "--user")
	}

	servicePath := filepath.Join(unitDir, *name+".service")
	timerPath := filepath.Join(unitDir, *name+".timer")
	serviceExists, err := fileExists(servicePath)
	if err != nil {
		return fmt.Errorf("inspect service unit: %w", err)
	}
	timerExists, err := fileExists(timerPath)
	if err != nil {
		return fmt.Errorf("inspect timer unit: %w", err)
	}
	if !serviceExists && !timerExists {
		return fmt.Errorf("no systemd units found for %q in %s", *name, unitDir)
	}

	if timerExists {
		args := append(append([]string{}, systemctlArgs...), "disable", "--now", *name+".timer")
		if err := runSystemctl(args...); err != nil {
			return err
		}
	}
	if serviceExists {
		args := append(append([]string{}, systemctlArgs...), "stop", *name+".service")
		if err := runSystemctl(args...); err != nil {
			return err
		}
	}
	if err := removeServiceUnits(unitDir, *name); err != nil {
		return err
	}
	if err := runSystemctl(append(append([]string{}, systemctlArgs...), "daemon-reload")...); err != nil {
		return err
	}

	scope := "user"
	if *system {
		scope = "system"
	}
	fmt.Printf("Uninstalled %s service and timer from %s\n", scope, unitDir)
	return nil
}

func writeServiceUnits(unitDir, executable, configPath, name string, interval time.Duration, calendar string, randomizedDelay time.Duration, system bool) error {
	if !validSystemdUnitName(name) {
		return fmt.Errorf("invalid systemd service name %q", name)
	}
	workingDirectory, err := filepath.Abs(filepath.Dir(configPath))
	if err != nil {
		return fmt.Errorf("resolve service working directory: %w", err)
	}
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return fmt.Errorf("create systemd user unit directory: %w", err)
	}
	serviceLines := []string{
		"[Unit]",
		"Description=URL Sync download job",
	}
	if system {
		serviceLines = append(serviceLines, "Requires=network-online.target", "After=network-online.target")
	}
	serviceLines = append(serviceLines,
		"",
		"[Service]",
		"Type=oneshot",
		"WorkingDirectory="+systemdQuote(workingDirectory),
		"ExecStart="+strings.Join([]string{
			systemdQuote(executable),
			systemdQuote("-config"),
			systemdQuote(configPath),
		}, " "),
		"TimeoutStartSec=infinity",
		"",
	)
	service := strings.Join(serviceLines, "\n")
	timerLines := []string{
		"[Unit]",
		"Description=Run URL Sync periodically",
		"",
		"[Timer]",
	}
	if calendar != "" {
		timerLines = append(timerLines, "OnCalendar="+strings.ReplaceAll(calendar, "%", "%%"))
	} else {
		timerLines = append(timerLines, "OnBootSec=1min", "OnUnitActiveSec="+systemdDuration(interval))
	}
	if randomizedDelay > 0 {
		timerLines = append(timerLines, "RandomizedDelaySec="+systemdDuration(randomizedDelay))
	}
	timerLines = append(timerLines,
		"Persistent=true",
		"Unit="+name+".service",
		"",
	)
	timerLines = append(timerLines,
		"[Install]",
		"WantedBy=timers.target",
		"",
	)
	timer := strings.Join(timerLines, "\n")
	serviceFile := name + ".service"
	servicePath := filepath.Join(unitDir, serviceFile)
	if err := writeNewFile(servicePath, []byte(service)); err != nil {
		return fmt.Errorf("write %s: %w", serviceFile, err)
	}
	timerFile := name + ".timer"
	timerPath := filepath.Join(unitDir, timerFile)
	if err := writeNewFile(timerPath, []byte(timer)); err != nil {
		if removeErr := os.Remove(servicePath); removeErr != nil {
			return errors.Join(fmt.Errorf("write %s: %w", timerFile, err), fmt.Errorf("remove newly created %s after failure: %w", serviceFile, removeErr))
		}
		return fmt.Errorf("write %s: %w", timerFile, err)
	}
	return nil
}

func removeServiceUnits(unitDir, name string) error {
	if !validSystemdUnitName(name) {
		return fmt.Errorf("invalid systemd service name %q", name)
	}
	paths := []string{
		filepath.Join(unitDir, name+".service"),
		filepath.Join(unitDir, name+".timer"),
	}
	removed := false
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", filepath.Base(path), err)
		} else if err == nil {
			removed = true
		}
	}
	if !removed {
		return fmt.Errorf("no systemd units found for %q in %s", name, unitDir)
	}
	return nil
}

func fileExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func writeNewFile(path string, content []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		closeErr := file.Close()
		if removeErr := os.Remove(path); removeErr != nil {
			return errors.Join(err, closeErr, fmt.Errorf("remove incomplete file: %w", removeErr))
		}
		return errors.Join(err, closeErr)
	}
	if err := file.Close(); err != nil {
		if removeErr := os.Remove(path); removeErr != nil {
			return errors.Join(err, fmt.Errorf("remove incomplete file: %w", removeErr))
		}
		return err
	}
	return nil
}

func validSystemdUnitName(name string) bool {
	return systemdUnitNamePattern.MatchString(name) &&
		!strings.HasSuffix(name, ".service") &&
		!strings.HasSuffix(name, ".timer")
}

func parseSystemdDuration(value string) (time.Duration, error) {
	if strings.Contains(value, "-") {
		return 0, fmt.Errorf("negative durations are not supported")
	}
	dayMatches := systemdDayPattern.FindAllStringSubmatchIndex(value, -1)
	var duration time.Duration
	for _, match := range dayMatches {
		days, err := strconv.ParseInt(value[match[2]:match[3]], 10, 64)
		if err != nil || days > int64((1<<63-1)/int64(24*time.Hour)) {
			return 0, fmt.Errorf("duration out of range")
		}
		dayDuration := time.Duration(days) * 24 * time.Hour
		if duration > time.Duration(1<<63-1)-dayDuration {
			return 0, fmt.Errorf("duration out of range")
		}
		duration += dayDuration
	}
	remainder := systemdDayPattern.ReplaceAllString(value, "")
	if remainder != "" {
		remainderDuration, err := time.ParseDuration(remainder)
		if err != nil {
			return 0, err
		}
		if duration > time.Duration(1<<63-1)-remainderDuration {
			return 0, fmt.Errorf("duration out of range")
		}
		duration += remainderDuration
	}
	return duration, nil
}

func systemdQuote(value string) string {
	return strings.ReplaceAll(strconv.Quote(value), "%", "%%")
}

var systemdDayPattern = regexp.MustCompile(`([0-9]+)d`)
var systemdUnitNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@-]*$`)

func systemdDuration(duration time.Duration) string {
	seconds := int64(duration / time.Second)
	var parts []string
	if days := seconds / 86400; days > 0 {
		parts = append(parts, strconv.FormatInt(days, 10)+"d")
		seconds %= 86400
	}
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

func notifyDownloadResult(ctx context.Context, client *http.Client, ntfyURL, ntfyPrefix string, file Download, updated bool, downloadErr error) error {
	if ntfyURL == "" || (!updated && downloadErr == nil) {
		return nil
	}

	message := fmt.Sprintf("File updated: %s", file.Destination)
	if downloadErr != nil {
		message = fmt.Sprintf("Download failed: %s", file.Destination)
	}
	message = ntfyPrefix + message
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
