package controller

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

const (
	probeSelector   = "sbm-probe-selector"
	unavailableNode = "sbm-unavailable"
)

func object(value any) map[string]any {
	m, _ := value.(map[string]any)
	return m
}

func array(value any) []any {
	a, _ := value.([]any)
	return a
}

func ensureObject(parent map[string]any, key string) map[string]any {
	child := object(parent[key])
	if child == nil {
		child = make(map[string]any)
		parent[key] = child
	}
	return child
}

func (c *controller) build(nodes []map[string]any) (map[string]any, error) {
	var policy map[string]any
	if err := readJSON(c.path("policy.json"), &policy); err != nil || policy == nil {
		return nil, errors.New("Install a valid system network policy first")
	}
	return c.buildPolicy(policy, nodes)
}

func (c *controller) buildPolicy(policy map[string]any, nodes []map[string]any) (map[string]any, error) {
	var group map[string]any
	kept := []any{}
	for _, value := range array(policy["outbounds"]) {
		outbound := object(value)
		switch outbound["type"] {
		case "selector", "urltest":
			if group != nil {
				return nil, errors.New("System policy must have one proxy group")
			}
			group = outbound
		case "direct", "block", "dns":
			kept = append(kept, outbound)
		}
	}
	if group == nil {
		return nil, errors.New("System policy must have one proxy group")
	}
	tag, ok := group["tag"].(string)
	if !ok || tag == "" {
		return nil, errors.New("System proxy group must have a tag")
	}
	used := map[string]bool{"sbm-direct": true, probeSelector: true, unavailableNode: true}
	if used[tag] {
		return nil, errors.New("System policy uses a reserved SBM outbound tag")
	}
	used[tag] = true
	for _, value := range kept {
		outboundTag, ok := object(value)["tag"].(string)
		if !ok || outboundTag == "" || used[outboundTag] {
			return nil, errors.New("System policy uses a reserved SBM outbound tag")
		}
		used[outboundTag] = true
	}
	members := make([]any, 0, len(nodes))
	for _, node := range nodes {
		nodeTag, ok := node["tag"].(string)
		if !ok || nodeTag == "" || used[nodeTag] {
			return nil, errors.New("System policy uses a reserved SBM outbound tag")
		}
		used[nodeTag] = true
		members = append(members, nodeTag)
	}
	if len(members) == 0 {
		return nil, errors.New("No subscription nodes to select")
	}
	delete(policy, "proxy_binding")
	trafficMembers := append([]any{unavailableNode}, members...)
	outbounds := []any{
		map[string]any{"type": "selector", "tag": tag, "outbounds": trafficMembers,
			"default": unavailableNode, "interrupt_exist_connections": true},
		map[string]any{"type": "selector", "tag": probeSelector, "outbounds": members},
		map[string]any{"type": "block", "tag": unavailableNode},
	}
	outbounds = append(outbounds, kept...)
	for _, node := range nodes {
		outbounds = append(outbounds, node)
	}
	outbounds = append(outbounds, map[string]any{"type": "direct", "tag": "sbm-direct"})
	policy["outbounds"] = outbounds
	route := ensureObject(policy, "route")
	requestedPhysical := route["auto_detect_interface"] == false
	route["auto_detect_interface"] = true
	for _, value := range array(route["rule_set"]) {
		ruleSet := object(value)
		if ruleSet["tag"] == "geoip-ru" {
			ruleSet["type"], ruleSet["format"], ruleSet["path"] = "local", "binary", c.path("ru.srs")
		}
	}
	rules := []any{
		map[string]any{"inbound": []string{"sbm-probe"}, "outbound": probeSelector},
		map[string]any{"inbound": []string{"sbm-uplink"}, "outbound": "sbm-direct"},
	}
	route["rules"] = append(rules, array(route["rules"])...)
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return nil, errors.New("Could not generate a probe secret")
	}
	secret := hex.EncodeToString(token)
	inbounds := array(policy["inbounds"])
	for _, inbound := range []struct {
		name string
		port int
	}{{"sbm-probe", probePort}, {"sbm-uplink", directPort}} {
		inbounds = append(inbounds, map[string]any{
			"type": "mixed", "tag": inbound.name, "listen": "127.0.0.1",
			"listen_port": inbound.port, "users": []any{map[string]any{"username": "sbm", "password": secret}},
		})
	}
	policy["inbounds"] = inbounds
	ensureObject(policy, "experimental")["clash_api"] = map[string]any{"external_controller": fmt.Sprintf("127.0.0.1:%d", apiPort), "secret": secret}
	uplink, err := c.bindPhysicalUplink(policy)
	if err != nil {
		return nil, err
	}
	if requestedPhysical && uplink.Interface == "" {
		return nil, errors.New("Policy requires physical uplink binding")
	}
	return policy, nil
}

type proxyConfig struct {
	Secret  string
	Traffic string
	Nodes   []string
}

func (c *controller) proxyConfig(generation string) (proxyConfig, error) {
	var config struct {
		Experimental struct {
			ClashAPI struct {
				Secret string
			} `json:"clash_api"`
		}
		Outbounds []struct {
			Type      string
			Tag       string
			Outbounds []string
		}
	}
	if err := readJSON(c.path(generation), &config); err != nil {
		return proxyConfig{}, errors.New("Could not read active SBM generation")
	}
	proxy := proxyConfig{Secret: config.Experimental.ClashAPI.Secret}
	if proxy.Secret == "" {
		return proxyConfig{}, errors.New("Active SBM generation has no probe secret")
	}
	for _, outbound := range config.Outbounds {
		if outbound.Type != "selector" {
			continue
		}
		if outbound.Tag == probeSelector {
			proxy.Nodes = outbound.Outbounds
		} else {
			proxy.Traffic = outbound.Tag
		}
	}
	return proxy, nil
}
