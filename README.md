# Sing-Box Manager

SBM manages a system-wide sing-box VPN on macOS. It validates named profiles, starts a single foreground or supervised background session, and selects a node using actual download checks. A background request survives reboot; an explicit stop clears that request.

## Install

Download the archive for your Mac from [SBM releases](https://github.com/d1ys3nk0/sbm/releases/latest), verify its SHA-256 digest against `checksums.txt`, and run `sudo ./sbm install`. The installer requires a separate sing-box installation. It installs `/opt/sbm/bin/sbm`, links `/usr/local/bin/sbm`, and registers the `io.github.d1ys3nk0.sbm` LaunchDaemon. `sudo ./sbm install --dry-run` previews installation. A new installation remains disconnected. `sudo sbm uninstall` stops the session and removes installation resources while preserving private data.

Private state lives under `/var/lib/sbm`, including named profiles, network policy, generated configuration, logs, and recovery records. The directory and its `profiles` subdirectory must be root-owned with mode `0700`; sensitive files use mode `0600`. The default sing-box path is `/opt/homebrew/bin/sing-box` on Apple Silicon and `/usr/local/bin/sing-box` on Intel. A root-owned `sing-box-bin.json` in the state directory may contain another absolute executable path as a JSON string.

## Profiles and sessions

```sh
sudo sbm subscribe main -
sudo sbm import office -
sudo sbm profiles
sudo sbm check main
sudo sbm start main
sudo sbm status --json
sudo sbm stop
sudo sbm run office
```

`subscribe NAME -` reads an HTTPS subscription URL from standard input. Subscription profiles use the host policy in `/var/lib/sbm/policy.json`; startup refreshes nodes and falls back to the last validated copy if refresh fails. `import NAME -` reads a local sing-box JSON configuration from standard input. It accepts a single selector containing every network node exactly once, with direct, block, and DNS outbounds preserved in the profile policy. Unsupported or ambiguous settings fail before the saved profile is changed. A local binary `geoip-ru` rule set with an absolute source path is converted to use `/var/lib/sbm/ru.srs`; transfer and validate that file separately before importing. The selector itself may contain only its type, tag, and outbounds. SBM validates the generated configuration with sing-box before saving the imported profile.

`start NAME` records background intent and accepts startup asynchronously. `run NAME` runs a foreground session without saving boot intent. `stop` stops either mode and clears background intent; `toggle NAME` starts a stopped profile or stops the matching session. Stop the current session before changing its name or mode. Status distinguishes a running process from a successful download-based node check. Profile files, subscription URLs, generated configurations and logs may contain private information.

For hosts requiring physical-uplink binding, the private `/var/lib/sbm/physical-uplink.json` may contain `true`. SBM selects an active Ethernet or Wi-Fi interface with IPv4, binds network and direct outbounds to it, and regenerates the session when the interface or its addresses change. An existing conflicting outbound binding is rejected.

`sudo sbm update-geoip` updates the applicable GeoIP rule set. SBM checks at startup, repeats successful checks after seven days, and retries failures hourly. During a session, a changed rule set is activated by restarting the owned sing-box process and verifying node connectivity and configured routes; failed activation restores the previous rules and retains a recovery journal if restoration cannot be confirmed. Configure optional private route checks in `/var/lib/sbm/route-checks.json` as a JSON array of objects containing `name`, `url`, and optional `expected_status` (default 200). These checks can cover proxy, direct, and tailnet destinations.

## Development and releases

Install Go 1.27.4 and run `go test -race ./...` and `go vet ./...`. A canonical version tag triggers the release workflow, which publishes Apple Silicon and Intel macOS archives and `checksums.txt` after checks pass.
