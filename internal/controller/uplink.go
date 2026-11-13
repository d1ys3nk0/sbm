package controller

import (
	"errors"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

type uplinkState struct {
	Interface string `json:"interface"`
	Addresses string `json:"addresses"`
}

var (
	hardwarePortPattern     = regexp.MustCompile(`(?m)^Hardware Port: (.+)$`)
	hardwareDevicePattern   = regexp.MustCompile(`(?m)^Device: (en[0-9]+)$`)
	interfaceIPv4Pattern    = regexp.MustCompile(`(?m)^\s*inet ([^\s]+)`)
	interfaceAddressPattern = regexp.MustCompile(`(?m)^\s*inet6? ([^\s]+)`)
	interfaceActivePattern  = regexp.MustCompile(`(?m)^\s*status: active\s*$`)
)

func (c *controller) physicalUplink() (uplinkState, error) {
	listed := c.run(15*time.Second, "", "/usr/sbin/networksetup", "-listallhardwareports")
	if listed.code != 0 {
		return uplinkState{}, errors.New("Could not inspect physical network interfaces")
	}
	type candidate struct {
		device string
		wifi   bool
	}
	var candidates []candidate
	for _, block := range strings.Split(listed.stdout, "\n\n") {
		port := hardwarePortPattern.FindStringSubmatch(block)
		device := hardwareDevicePattern.FindStringSubmatch(block)
		if len(port) != 2 || len(device) != 2 {
			continue
		}
		if strings.Contains(port[1], "Ethernet") || port[1] == "Wi-Fi" {
			candidates = append(candidates, candidate{device[1], port[1] == "Wi-Fi"})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return !candidates[i].wifi && candidates[j].wifi })
	for _, candidate := range candidates {
		result := c.run(15*time.Second, "", "/sbin/ifconfig", candidate.device)
		if result.code != 0 || !interfaceActivePattern.MatchString(result.stdout) || !interfaceIPv4Pattern.MatchString(result.stdout) {
			continue
		}
		var addresses []string
		for _, match := range interfaceAddressPattern.FindAllStringSubmatch(result.stdout, -1) {
			addresses = append(addresses, match[1])
		}
		sort.Strings(addresses)
		return uplinkState{candidate.device, strings.Join(addresses, "|")}, nil
	}
	return uplinkState{}, errors.New("No active Ethernet or Wi-Fi interface with IPv4")
}

func (c *controller) configuredUplink() (uplinkState, error) {
	var enabled bool
	if err := readJSON(c.path("physical-uplink.json"), &enabled); errors.Is(err, os.ErrNotExist) {
		return uplinkState{}, nil
	} else if err != nil {
		return uplinkState{}, errors.New("Invalid physical uplink configuration")
	}
	if !enabled {
		return uplinkState{}, nil
	}
	return c.physicalUplink()
}

func (c *controller) bindPhysicalUplink(config map[string]any) (uplinkState, error) {
	uplink, err := c.configuredUplink()
	if err != nil {
		return uplinkState{}, err
	}
	if uplink.Interface == "" {
		return uplinkState{}, nil
	}
	for _, value := range array(config["outbounds"]) {
		outbound := object(value)
		_, networkNode := outbound["server"]
		if outbound["type"] != "direct" && !networkNode {
			continue
		}
		if existing, present := outbound["bind_interface"]; present && existing != uplink.Interface {
			return uplinkState{}, errors.New("Outbound interface conflicts with the physical uplink")
		}
		outbound["bind_interface"] = uplink.Interface
	}
	ensureObject(config, "route")["auto_detect_interface"] = false
	return uplink, nil
}
