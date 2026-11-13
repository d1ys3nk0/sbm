package controller

import (
	"context"
	"testing"
)

func TestPhysicalUplinkPrefersEthernetAndTracksAddresses(t *testing.T) {
	c := testController(t)
	c.runCommand = func(_ context.Context, _, executable string, args ...string) result {
		switch executable {
		case "/usr/sbin/networksetup":
			return result{stdout: "Hardware Port: Wi-Fi\nDevice: en0\n\nHardware Port: USB Ethernet\nDevice: en5\n"}
		case "/sbin/ifconfig":
			if args[0] == "en5" {
				return result{stdout: "inet 192.0.2.4 netmask 0xffffff00\ninet6 fe80::1%en5 prefixlen 64\nstatus: active\n"}
			}
			return result{stdout: "inet 192.0.2.3 netmask 0xffffff00\nstatus: active\n"}
		}
		return result{code: 1}
	}
	uplink, err := c.physicalUplink()
	if err != nil || uplink.Interface != "en5" || uplink.Addresses != "192.0.2.4|fe80::1%en5" {
		t.Fatalf("uplink = %+v, %v", uplink, err)
	}
}

func TestPhysicalUplinkRejectsInactiveAndMissingIPv4(t *testing.T) {
	c := testController(t)
	c.runCommand = func(_ context.Context, _, executable string, args ...string) result {
		if executable == "/usr/sbin/networksetup" {
			return result{stdout: "Hardware Port: Wi-Fi\nDevice: en0\n"}
		}
		return result{stdout: "inet6 fe80::1 prefixlen 64\nstatus: active\n"}
	}
	if _, err := c.physicalUplink(); err == nil {
		t.Fatal("accepted interface without IPv4")
	}
}

func TestBindPhysicalUplinkRequiresExplicitConfiguration(t *testing.T) {
	c := testController(t)
	config := map[string]any{"outbounds": []any{map[string]any{"type": "direct", "tag": "direct"}, map[string]any{"type": "vless", "server": "example.org"}}, "route": map[string]any{"auto_detect_interface": true}}
	if _, err := c.bindPhysicalUplink(config); err != nil {
		t.Fatal(err)
	}
	if config["route"].(map[string]any)["auto_detect_interface"] != true {
		t.Fatal("unexpected uplink activation")
	}
	if err := writeJSON(c.path("physical-uplink.json"), true); err != nil {
		t.Fatal(err)
	}
	c.runCommand = func(_ context.Context, _, executable string, args ...string) result {
		if executable == "/usr/sbin/networksetup" {
			return result{stdout: "Hardware Port: Wi-Fi\nDevice: en0\n"}
		}
		return result{stdout: "inet 192.0.2.1 netmask 0xffffff00\nstatus: active\n"}
	}
	if _, err := c.bindPhysicalUplink(config); err != nil {
		t.Fatal(err)
	}
	if config["route"].(map[string]any)["auto_detect_interface"] != false {
		t.Fatal("physical binding did not disable autodetection")
	}
	for _, value := range config["outbounds"].([]any) {
		if value.(map[string]any)["bind_interface"] != "en0" {
			t.Fatal("outbound not bound")
		}
	}
}
