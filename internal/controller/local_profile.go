package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/d1ys3nk0/sbm/internal/inputs"
)

type savedProfile struct {
	inputs.Snapshot
	Policy map[string]any `json:"policy,omitempty"`
}

func (c *controller) buildProfile(profile savedProfile) (map[string]any, error) {
	if profile.Policy == nil {
		return c.build(profile.Nodes)
	}
	data, err := json.Marshal(profile.Policy)
	if err != nil {
		return nil, err
	}
	var policy map[string]any
	if err := json.Unmarshal(data, &policy); err != nil {
		return nil, err
	}
	return c.buildPolicy(policy, profile.Nodes)
}

func (c *controller) importProfile(name string, source io.Reader) error {
	path, err := c.sourcePath(name)
	if err != nil {
		return err
	}
	local, err := inputs.ConvertLocalProfile(source)
	if err != nil {
		return err
	}
	return c.locked(func() error {
		profile := savedProfile{Snapshot: inputs.Snapshot{Nodes: local.Nodes}, Policy: local.Policy}
		var existing savedProfile
		err := readJSON(path, &existing)
		if err == nil {
			want, wantErr := json.Marshal(profile)
			have, haveErr := json.Marshal(existing)
			if wantErr == nil && haveErr == nil && bytes.Equal(want, have) {
				return nil
			}
			return errors.New("A different profile already exists under that name")
		}
		if !errors.Is(err, os.ErrNotExist) {
			return errors.New("Could not read existing profile")
		}
		config, err := c.buildProfile(profile)
		if err != nil {
			return err
		}
		if _, err = c.publish(name, config, true); err != nil {
			return err
		}
		return writeJSON(path, profile)
	})
}
