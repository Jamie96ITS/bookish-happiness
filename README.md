# urlsync

`urlsync` downloads HTTP and HTTPS resources listed in a JSON settings file. Each entry maps a URL to a local destination. Existing files are checked with `If-Modified-Since`; a server `304 Not Modified` response or a `Last-Modified` time that is not newer leaves the local file untouched.

When a response includes `Last-Modified`, the downloaded file receives that modification time. If the server omits the header, a successful response is downloaded because there is no server timestamp to compare.

## Settings

Edit `settings.json`:

```json
{
  "ntfy_url": "https://ntfy.sh/YOUR_TOPIC",
  "ntfy_prefix": "[urlsync] ",
  "on_success": {
    "command": "systemctl",
    "args": ["restart", "nginx"]
  },
  "files": [
    {
      "url": "https://example.com/archive.zip",
      "destination": "downloads/archive.zip",
      "headers": {
        "Authorization": "Bearer YOUR_TOKEN",
        "X-API-Key": "YOUR_API_KEY"
      }
    }
  ]
}
```

Destinations may be relative to the current working directory or absolute paths. Parent directories are created as needed.
The optional `headers` object sends the listed HTTP headers with that URL's request. Store credentials outside version control and replace the example placeholders before use. `urlsync` sets `If-Modified-Since` itself when a destination already exists.

Set the optional global `ntfy_url` to your ntfy.sh topic URL to receive a message when a file is updated or a download fails. No notification is sent when a file is unchanged. Leave it empty to disable notifications. Notification delivery errors are logged locally.

The optional global `ntfy_prefix` is prepended literally to every ntfy message, including update and error notices. Include any desired spacing in the prefix; it defaults to an empty string.

Set the optional global `on_success` command to run a program after all downloads succeed and at least one file is updated. For example, the configuration above restarts nginx. The command runs once per invocation, directly without a shell; use `command` for the executable and `args` for its arguments. It is not run when files are unchanged or any download fails. A non-zero command exit causes `urlsync` to exit with an error.

## Run

Install Go 1.22 or newer, then run:

```sh
go run . -config settings.json
```

Build a native executable with `go build -o urlsync .`. Go can also cross-compile for other supported platforms, for example:

```sh
GOOS=windows GOARCH=amd64 go build -o urlsync.exe .
GOOS=darwin GOARCH=arm64 go build -o urlsync .
GOOS=linux GOARCH=amd64 go build -o urlsync .
```

Publishing a GitHub Release runs the release workflow, which tests the app, builds binaries for Linux, macOS, and Windows on amd64 and arm64, and uploads them with `SHA256SUMS.txt` to the release.

## Linux systemd user timer

On Linux with systemd, run the built executable from the directory containing `settings.json`:

```sh
./urlsync install-service --config settings.json --interval 15m
```

This writes `urlsync.service` and `urlsync.timer` under `~/.config/systemd/user`, reloads the user systemd manager, and enables the timer. The service is a one-shot job, first scheduled one minute after boot and then at the configured interval. The command runs only when a download changes a file and all configured downloads succeed. This installs a user service; to keep it running when logged out, enable lingering with `loginctl enable-linger "$USER"`.

## AI Disclosure and Disclaimer

This project was wholly generated with GitHub Copilot, an AI assistant, for personal use. GitHub Copilot is credited as the AI author and contributor.

This software is provided "as is," without guarantees or warranties of any kind, express or implied. It may contain errors and may not be suitable, secure, or reliable for any particular purpose. Use it at your own risk.