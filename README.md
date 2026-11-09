# Sing-Box Manager

SBM manages a system-wide [sing-box](https://sing-box.sagernet.org/) VPN on macOS.
It saves subscriptions and connection intent, validates generated configurations,
and uses system LaunchDaemons to restore and monitor a connection before login.

## Install

Download the archive for your Mac from the
[latest release](https://github.com/d1ys3nk0/sbm/releases/latest), verify its
SHA-256 digest against `checksums.txt`, and install the executable as root at
`/usr/local/bin/sbm`. SBM requires a separate sing-box installation. The Mac
provisioning role installs the two system LaunchDaemon definitions and provisions
the private host network policy.

SBM stores private state under `/Library/Application Support/apexplane-sbm`.
The directory and its contents must be accessible only to root. The default
sing-box path is `/opt/homebrew/bin/sing-box` on Apple Silicon and
`/usr/local/bin/sing-box` on Intel. To use another executable, place its absolute
path as a JSON string in the root-owned `sing-box-bin.json` file in that
directory.

## Manage a connection

Run system commands with `sudo`. A private host policy must be installed before
checking or connecting a subscription.

```sh
sudo sbm subscribe main -
sudo sbm subscriptions
sudo sbm check main
sudo sbm connect main
sudo sbm status
sudo sbm stop
```

For `subscribe main -`, enter the subscription URL on standard input. SBM also
accepts a URL as the final argument, but that can expose it in shell history and
process listings. The subscription and generated configuration contain private
connection details.

The watchdog reconciles saved connection intent at boot, checks health every
30 seconds, and limits automatic recovery attempts. `stop` clears the saved
intent. Run `sudo sbm update-geoip` to refresh the local GeoIP rule set when
the policy uses it.

## Development and releases

Install Go 1.27.3 and run `go test -race ./...` and `go vet ./...`.
A canonical version tag triggers the release workflow after checks pass.
Releases contain macOS Apple Silicon and Intel archives and `checksums.txt`.
