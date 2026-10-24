package main

import (
	"bytes"
	"context"
	"encoding/binary"
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
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
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
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		return fmt.Errorf("install-service is only supported on Linux with systemd or Windows Task Scheduler")
	}

	flags := flag.NewFlagSet("install-service", flag.ContinueOnError)
	configPath := flags.String("config", "settings.json", "path to the JSON settings file")
	name := flags.String("name", "urlsync", "base name for the generated service or scheduled task")
	interval := flags.String("interval", "", "time between runs (for example 15m, 1h, or 2d)")
	onCalendar := flags.String("on-calendar", "", "calendar schedule (for example daily or Mon *-*-* 10:00:00)")
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
	if runtime.GOOS == "windows" && *system {
		return fmt.Errorf("--system is not supported on Windows; tasks are installed for the current user")
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
	if runtime.GOOS == "windows" {
		return installWindowsTask(*name, executable, absoluteConfig, intervalDuration, calendar, randomizedDelayDuration)
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
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		return fmt.Errorf("uninstall-service is only supported on Linux with systemd or Windows Task Scheduler")
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
	if runtime.GOOS == "windows" {
		if *system {
			return fmt.Errorf("--system is not supported on Windows; tasks are installed for the current user")
		}
		if err := runSchtasks("/Delete", "/TN", windowsTaskName(*name), "/F"); err != nil {
			return err
		}
		fmt.Printf("Uninstalled current-user scheduled task %q\n", *name)
		return nil
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

func installWindowsTask(name, executable, configPath string, interval time.Duration, calendar string, randomizedDelay time.Duration) (returnErr error) {
	taskXML, err := windowsTaskXML(name, executable, configPath, interval, calendar, randomizedDelay, time.Now())
	if err != nil {
		return err
	}
	taskName := windowsTaskName(name)
	if err := runSchtasks("/Query", "/TN", taskName); err == nil {
		return fmt.Errorf("scheduled task %q already exists; uninstall it before installing again", name)
	}
	file, err := os.CreateTemp("", "urlsync-task-*.xml")
	if err != nil {
		return fmt.Errorf("create scheduled task definition: %w", err)
	}
	tempPath := file.Name()
	defer func() {
		if removeErr := os.Remove(tempPath); removeErr != nil && !os.IsNotExist(removeErr) {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove temporary scheduled task definition: %w", removeErr))
		}
	}()
	if _, err := file.Write(taskXML); err != nil {
		closeErr := file.Close()
		return errors.Join(fmt.Errorf("write scheduled task definition: %w", err), closeErr)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close scheduled task definition: %w", err)
	}
	if err := runSchtasks("/Create", "/TN", taskName, "/XML", tempPath); err != nil {
		return err
	}
	fmt.Printf("Installed current-user scheduled task %q\n", name)
	return nil
}

func windowsTaskXML(name, executable, configPath string, interval time.Duration, calendar string, randomizedDelay time.Duration, now time.Time) ([]byte, error) {
	if !validSystemdUnitName(name) {
		return nil, fmt.Errorf("invalid scheduled task name %q", name)
	}
	account, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("locate current user for scheduled task: %w", err)
	}
	startBoundary, schedule, repetition, err := windowsTaskSchedule(interval, calendar, randomizedDelay, now)
	if err != nil {
		return nil, err
	}
	trigger := "<CalendarTrigger>"
	if repetition != "" {
		trigger += repetition
	}
	trigger += "<StartBoundary>" + xmlText(startBoundary) + "</StartBoundary><Enabled>true</Enabled>" + schedule
	if randomizedDelay > 0 {
		trigger += "<RandomDelay>" + isoDuration(randomizedDelay) + "</RandomDelay>"
	}
	trigger += "</CalendarTrigger>"
	arguments := windowsQuoteArgument("-config") + " " + windowsQuoteArgument(configPath)
	document := `<?xml version="1.0" encoding="UTF-16"?>` +
		`<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">` +
		`<RegistrationInfo><Description>URL Sync scheduled download job</Description></RegistrationInfo>` +
		`<Triggers>` + trigger + `</Triggers>` +
		`<Principals><Principal id="Author"><UserId>` + xmlText(account.Username) + `</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>` +
		`<Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy><StartWhenAvailable>true</StartWhenAvailable><Enabled>true</Enabled><ExecutionTimeLimit>PT0S</ExecutionTimeLimit></Settings>` +
		`<Actions Context="Author"><Exec><Command>` + xmlText(executable) + `</Command><Arguments>` + xmlText(arguments) + `</Arguments><WorkingDirectory>` + xmlText(filepath.Dir(configPath)) + `</WorkingDirectory></Exec></Actions>` +
		`</Task>`
	return encodeUTF16XML(document), nil
}

func windowsTaskSchedule(interval time.Duration, calendar string, randomizedDelay time.Duration, now time.Time) (string, string, string, error) {
	if randomizedDelay < 0 || randomizedDelay%time.Second != 0 {
		return "", "", "", fmt.Errorf("randomized delay must be a non-negative whole number of seconds")
	}
	startDate := now.Format("2006-01-02")
	if calendar != "" {
		weekdays, dayOfMonth, month, timeOfDay, kind, err := parseWindowsCalendar(calendar)
		if err != nil {
			return "", "", "", err
		}
		startBoundary := startDate + "T" + timeOfDay
		switch kind {
		case "day":
			return startBoundary, "<ScheduleByDay><DaysInterval>1</DaysInterval></ScheduleByDay>", "", nil
		case "hour":
			repetition := "<Repetition><Interval>PT1H</Interval><Duration>P1D</Duration><StopAtDurationEnd>false</StopAtDurationEnd></Repetition>"
			return startBoundary, "<ScheduleByDay><DaysInterval>1</DaysInterval></ScheduleByDay>", repetition, nil
		case "week":
			var days strings.Builder
			for _, weekday := range weekdays {
				fmt.Fprintf(&days, "<%s/>", weekday)
			}
			return startBoundary, "<ScheduleByWeek><WeeksInterval>1</WeeksInterval><DaysOfWeek>" + days.String() + "</DaysOfWeek></ScheduleByWeek>", "", nil
		case "month":
			var months strings.Builder
			for _, monthName := range []string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"} {
				fmt.Fprintf(&months, "<%s/>", monthName)
			}
			return startBoundary, "<ScheduleByMonth><DaysOfMonth><Day>" + strconv.Itoa(dayOfMonth) + "</Day></DaysOfMonth><Months>" + months.String() + "</Months></ScheduleByMonth>", "", nil
		case "year":
			var months strings.Builder
			fmt.Fprintf(&months, "<%s/>", time.Month(month).String())
			return startBoundary, "<ScheduleByMonth><DaysOfMonth><Day>" + strconv.Itoa(dayOfMonth) + "</Day></DaysOfMonth><Months>" + months.String() + "</Months></ScheduleByMonth>", "", nil
		default:
			return "", "", "", fmt.Errorf("unsupported Windows calendar schedule")
		}
	}

	if interval < time.Minute || interval%time.Minute != 0 {
		return "", "", "", fmt.Errorf("Windows Task Scheduler intervals must be at least one whole minute")
	}
	startBoundary := now.Add(time.Minute).Format("2006-01-02T15:04:05")
	if interval%(24*time.Hour) == 0 {
		days := int(interval / (24 * time.Hour))
		if days > 365 {
			return "", "", "", fmt.Errorf("Windows Task Scheduler supports intervals up to 365 whole days")
		}
		return startBoundary, "<ScheduleByDay><DaysInterval>" + strconv.Itoa(days) + "</DaysInterval></ScheduleByDay>", "", nil
	}
	if interval < 24*time.Hour {
		minutes := int(interval / time.Minute)
		repetition := "<Repetition><Interval>PT" + strconv.Itoa(minutes) + "M</Interval><Duration>P1D</Duration><StopAtDurationEnd>false</StopAtDurationEnd></Repetition>"
		return startBoundary, "<ScheduleByDay><DaysInterval>1</DaysInterval></ScheduleByDay>", repetition, nil
	}
	return "", "", "", fmt.Errorf("Windows Task Scheduler cannot represent interval %s; use a whole number of days or an interval under 24 hours", interval)
}

func parseWindowsCalendar(calendar string) ([]string, int, int, string, string, error) {
	calendar = strings.TrimSpace(calendar)
	aliases := map[string]string{
		"hourly": "hourly", "daily": "daily", "midnight": "midnight",
		"weekly": "weekly", "monthly": "monthly", "yearly": "yearly",
		"annually": "yearly",
	}
	if alias, ok := aliases[strings.ToLower(calendar)]; ok {
		switch alias {
		case "hourly":
			return nil, 0, 0, "00:00:00", "hour", nil
		case "daily", "midnight":
			return nil, 0, 0, "00:00:00", "day", nil
		case "weekly":
			return []string{"Monday"}, 0, 0, "00:00:00", "week", nil
		case "monthly":
			return nil, 1, 0, "00:00:00", "month", nil
		case "yearly":
			return nil, 1, 1, "00:00:00", "year", nil
		}
	}

	fields := strings.Fields(calendar)
	var weekdayFields []string
	dateField, timeField := "", ""
	if len(fields) == 2 {
		dateField, timeField = fields[0], fields[1]
	} else if len(fields) == 3 {
		weekdayFields, dateField, timeField = strings.Split(fields[0], ","), fields[1], fields[2]
	} else {
		return nil, 0, 0, "", "", fmt.Errorf("unsupported Windows calendar expression %q; supported forms include daily, weekly, monthly, yearly, and weekday/date/time expressions", calendar)
	}
	if len(weekdayFields) > 0 {
		weekdays := make([]string, 0, len(weekdayFields))
		for _, weekday := range weekdayFields {
			day, ok := windowsWeekdays[weekday]
			if !ok {
				return nil, 0, 0, "", "", fmt.Errorf("unsupported weekday %q in calendar expression", weekday)
			}
			weekdays = append(weekdays, day)
		}
		timeOfDay, err := parseCalendarTime(timeField)
		if err != nil {
			return nil, 0, 0, "", "", err
		}
		if dateField != "*-*-*" {
			return nil, 0, 0, "", "", fmt.Errorf("weekday calendar schedules require *-*-* date")
		}
		return weekdays, 0, 0, timeOfDay, "week", nil
	}

	dateParts := strings.Split(dateField, "-")
	if len(dateParts) != 3 || dateParts[0] != "*" {
		return nil, 0, 0, "", "", fmt.Errorf("unsupported date pattern %q in calendar expression", dateField)
	}
	timeOfDay, err := parseCalendarTime(timeField)
	if err != nil {
		return nil, 0, 0, "", "", err
	}
	if dateParts[1] == "*" && dateParts[2] == "*" {
		return nil, 0, 0, timeOfDay, "day", nil
	}
	day, err := strconv.Atoi(dateParts[2])
	if err != nil || day < 1 || day > 31 {
		return nil, 0, 0, "", "", fmt.Errorf("unsupported day of month %q in calendar expression", dateParts[2])
	}
	if dateParts[1] == "*" {
		return nil, day, 0, timeOfDay, "month", nil
	}
	month, err := strconv.Atoi(dateParts[1])
	if err != nil || month < 1 || month > 12 {
		return nil, 0, 0, "", "", fmt.Errorf("unsupported month %q in calendar expression", dateParts[1])
	}
	return nil, day, month, timeOfDay, "year", nil
}

var windowsWeekdays = map[string]string{
	"Mon": "Monday", "Tue": "Tuesday", "Wed": "Wednesday", "Thu": "Thursday",
	"Fri": "Friday", "Sat": "Saturday", "Sun": "Sunday",
}

func parseCalendarTime(value string) (string, error) {
	if len(value) == 5 {
		value += ":00"
	}
	if _, err := time.Parse("15:04:05", value); err != nil {
		return "", fmt.Errorf("unsupported time %q in calendar expression; expected HH:MM or HH:MM:SS", value)
	}
	return value, nil
}

func isoDuration(duration time.Duration) string {
	seconds := int64(duration / time.Second)
	var result strings.Builder
	result.WriteByte('P')
	if days := seconds / 86400; days > 0 {
		fmt.Fprintf(&result, "%dD", days)
		seconds %= 86400
	}
	if seconds > 0 || result.Len() == 1 {
		result.WriteByte('T')
		if hours := seconds / 3600; hours > 0 {
			fmt.Fprintf(&result, "%dH", hours)
			seconds %= 3600
		}
		if minutes := seconds / 60; minutes > 0 {
			fmt.Fprintf(&result, "%dM", minutes)
			seconds %= 60
		}
		if seconds > 0 {
			fmt.Fprintf(&result, "%dS", seconds)
		}
	}
	return result.String()
}

func windowsTaskName(name string) string {
	return `\` + name
}

func windowsQuoteArgument(value string) string {
	if value == "" {
		return `""`
	}
	if !strings.ContainsAny(value, " \t\"") {
		return value
	}
	var result strings.Builder
	result.WriteByte('"')
	backslashes := 0
	for _, char := range value {
		switch char {
		case '\\':
			backslashes++
		case '"':
			result.WriteString(strings.Repeat(`\`, backslashes*2+1))
			result.WriteRune(char)
			backslashes = 0
		default:
			result.WriteString(strings.Repeat(`\`, backslashes))
			backslashes = 0
			result.WriteRune(char)
		}
	}
	result.WriteString(strings.Repeat(`\`, backslashes*2))
	result.WriteByte('"')
	return result.String()
}

func xmlText(value string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&apos;",
	).Replace(value)
}

func encodeUTF16XML(document string) []byte {
	encoded := utf16.Encode([]rune(document))
	result := make([]byte, 2+len(encoded)*2)
	result[0], result[1] = 0xff, 0xfe
	for i, codeUnit := range encoded {
		binary.LittleEndian.PutUint16(result[2+i*2:], codeUnit)
	}
	return result
}

func runSchtasks(args ...string) error {
	command := exec.Command("schtasks.exe", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks %s failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
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
