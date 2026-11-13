package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"time"
)

type statusReply struct {
	State          string            `json:"state"`
	Mode           string            `json:"mode"`
	Profile        string            `json:"profile"`
	DesiredProfile string            `json:"desired_profile"`
	Health         string            `json:"health"`
	Error          string            `json:"error"`
	Details        map[string]string `json:"details"`
}

func (c *controller) request(ctx context.Context, req Request) (statusReply, error) {
	control, err := LockControl(ctx, c.root)
	if err != nil {
		return statusReply{}, err
	}
	defer control.Close()
	for _, socket := range []string{"foreground.sock", "control.sock"} {
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", filepath.Join(c.root, socket))
		if err != nil {
			continue
		}
		defer conn.Close()
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		defer stop()
		if err := json.NewEncoder(conn).Encode(req); err != nil {
			return statusReply{}, err
		}
		if req.Action == "update-geoip" {
			control.Close()
		}
		var status statusReply
		if err := json.NewDecoder(conn).Decode(&status); err != nil {
			return statusReply{}, err
		}
		if req.Action != "status" && status.Error != "" {
			return statusReply{}, errors.New(status.Error)
		}
		return status, nil
	}
	if req.Action == "update-geoip" {
		session, err := lockSession(ctx, c.root)
		if err != nil {
			return statusReply{}, err
		}
		defer session.Close()
		control.Close()
		return statusReply{}, c.updateIdleGeoIP(ctx, false)
	}
	if req.Action != "status" && req.Action != "stop" {
		return statusReply{}, errors.New("Cannot contact the system SBM service")
	}
	var status Status
	if req.Action == "status" {
		status, err = OfflineStatus(c.root)
	} else {
		status, err = StopOffline(ctx, c.root)
		if err == nil {
			err = c.recoverStopped()
		}
	}
	if err != nil {
		return statusReply{}, err
	}
	data, err := json.Marshal(status)
	if err != nil {
		return statusReply{}, err
	}
	var reply statusReply
	err = json.Unmarshal(data, &reply)
	return reply, err
}
