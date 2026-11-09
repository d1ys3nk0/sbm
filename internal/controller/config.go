package controller

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
	used := map[string]bool{tag: true, "sbm-direct": true}
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
	delete(policy, "proxy_binding")
	outbounds := []any{map[string]any{"type": "urltest", "tag": tag, "outbounds": members, "url": checkURL, "interval": "1m", "tolerance": 50, "interrupt_exist_connections": false}}
	outbounds = append(outbounds, kept...)
	for _, node := range nodes {
		outbounds = append(outbounds, node)
	}
	outbounds = append(outbounds, map[string]any{"type": "direct", "tag": "sbm-direct"})
	policy["outbounds"] = outbounds
	route := ensureObject(policy, "route")
	route["auto_detect_interface"] = true
	for _, value := range array(route["rule_set"]) {
		ruleSet := object(value)
		if ruleSet["tag"] == "geoip-ru" {
			ruleSet["type"], ruleSet["format"], ruleSet["path"] = "local", "binary", c.path("ru.srs")
		}
	}
	rules := []any{
		map[string]any{"inbound": []string{"sbm-probe"}, "outbound": tag},
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
	return policy, nil
}

func (c *controller) apiSecret(generation string) (string, error) {
	var config map[string]any
	if err := readJSON(c.path(generation), &config); err != nil {
		return "", errors.New("Could not read active SBM generation")
	}
	secret, ok := object(object(config["experimental"])["clash_api"])["secret"].(string)
	if !ok || secret == "" {
		return "", errors.New("Active SBM generation has no probe secret")
	}
	return secret, nil
}
