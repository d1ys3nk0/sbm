# Sing-Box Manager

SBM manages a system-wide sing-box VPN on macOS. It turns a saved subscription and a host network policy into a validated sing-box configuration, restores the requested connection at boot, and switches away from nodes that stall during downloads.

## Why use SBM?

With the bare `sing-box run` CLI, you supply the configuration and arrange service startup, subscription refreshes, and recovery. SBM handles those jobs through a small set of commands. Sing-box still carries the traffic and applies the host's routing policy.

- **Subscription management.** Save named HTTPS subscriptions. `connect` refreshes the node list and can fall back to the last saved list if the refresh fails or the new configuration is rejected. `check` validates the saved nodes with the host policy without activating the configuration.
- **Boot and recovery.** System LaunchDaemons restore the selected connection before login and run the watchdog every 30 seconds. `stop` clears the saved connection so it stays off across reboots.
- **Download-based failover.** SBM sends VPN traffic through a node only after it passes a real download check. `status` shows the requested subscription, whether sing-box is running, the last probe result, and the selected node when available.
- **Optional host integration.** If configured, SBM clears the Tailscale exit node before starting sing-box and restores it on stop. The watchdog also refreshes the local GeoIP rule set when the host policy uses it.

## How node selection works

Sing-box 1.14.4's built-in `urltest` times a `HEAD` request. A node can answer it quickly and still stall when an app downloads the response body. Using a larger file URL with `urltest` still sends a `HEAD` request. [sing-box 1.14.4 source](https://github.com/SagerNet/sing-box/blob/v1.14.4/common/urltest/urltest.go#L119-L145)

SBM uses a separate sing-box selector for probes. It starts two HTTPS downloads through the same node using macOS's built-in `curl`, one from Cloudflare and one from OVHcloud. Either provider can pass the check by returning all 32 KiB within a shared three-second deadline. The first complete response cancels the other download.

A passing node stays active. If both probes fail, SBM disconnects VPN traffic from that node and checks other nodes in small batches. It switches to the first one that passes without restarting sing-box. Until a node passes, traffic assigned to the VPN route is blocked.

## Install

Download the archive for your Mac, verify its SHA-256 digest against `checksums.txt`, and install the executable as root at `/usr/local/bin/sbm`. [Latest release](https://github.com/d1ys3nk0/sbm/releases/latest)

SBM also requires a separate sing-box installation and host provisioning. The Mac provisioning role installs the two system LaunchDaemons and the private host network policy. [sing-box documentation](https://sing-box.sagernet.org/)

SBM stores private state under `/Library/Application Support/apexplane-sbm`. The directory and its contents must be accessible only to root. The default sing-box path is `/opt/homebrew/bin/sing-box` on Apple Silicon and `/usr/local/bin/sing-box` on Intel. To use another executable, place its absolute path as a JSON string in the root-owned `sing-box-bin.json` file in that directory.

## Manage a connection

Run system commands with `sudo` after provisioning the host.

```sh
sudo sbm subscribe main -
sudo sbm subscriptions
sudo sbm check main
sudo sbm connect main
sudo sbm status
```

For `subscribe main -`, enter the subscription URL on standard input. A URL supplied as the final argument can appear in shell history and process listings. The saved subscription and generated configuration contain private connection details.

Run `sudo sbm stop` to disconnect and clear the saved connection. Run `sudo sbm update-geoip` to refresh the local GeoIP rule set when the host policy uses it.

If your saved configuration predates automatic node switching, run `sudo sbm connect <subscription>` after upgrading to regenerate it. Older saved configurations use restart-based recovery until regenerated.

## Development and releases

Install Go 1.27.3 and run `go test -race ./...` and `go vet ./...`. A canonical version tag triggers the release workflow, which publishes Apple Silicon and Intel macOS archives and `checksums.txt` after checks pass.
