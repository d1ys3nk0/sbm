package controller

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

type routeCheck struct {
	Name           string `json:"name"`
	URL            string `json:"url"`
	ExpectedStatus int    `json:"expected_status,omitempty"`
}

func (c *controller) checkRoutes(ctx context.Context) error {
	var checks []routeCheck
	if err := readJSON(c.path("route-checks.json"), &checks); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return errors.New("Invalid route check configuration")
	}
	if len(checks) == 0 || len(checks) > 16 {
		return errors.New("Invalid route check configuration")
	}
	seen := make(map[string]bool)
	for _, check := range checks {
		u, err := url.Parse(check.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || strings.ContainsAny(check.URL, "\r\n\x00") || !namePattern.MatchString(check.Name) || seen[check.Name] {
			return errors.New("Invalid route check configuration")
		}
		seen[check.Name] = true
		status := check.ExpectedStatus
		if status == 0 {
			status = 200
		}
		if status < 100 || status > 599 {
			return errors.New("Invalid route check configuration")
		}
		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		result := c.runCommand(checkCtx, fmt.Sprintf("url = %q\n", check.URL), "/usr/bin/curl", "-q", "--config", "-", "-sS", "--max-time", "8", "--noproxy", "*", "-o", "/dev/null", "-w", "%{http_code}")
		cancel()
		if result.code != 0 || result.stdout != fmt.Sprintf("%03d", status) {
			return errors.New("Configured route check failed")
		}
	}
	return nil
}
