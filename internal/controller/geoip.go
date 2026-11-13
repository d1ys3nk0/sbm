package controller

import (
	"context"

	"github.com/d1ys3nk0/sbm/internal/inputs"
)

func (c *controller) updateGeoIP(ctx context.Context, scheduled bool, policy map[string]any, activate func() error) error {
	if c.geoipUpdate != nil {
		return c.geoipUpdate(ctx, scheduled, policy, activate)
	}
	return inputs.UpdateGeoIPForPolicyContext(ctx, c.root, c.bin, scheduled, policy, activate)
}

func (c *controller) updateIdleGeoIP(ctx context.Context, scheduled bool) error {
	if err := c.pendingSession(); err != nil {
		return err
	}
	policy, err := inputs.GeoIPPolicy(c.root)
	if err != nil {
		return err
	}
	return c.updateGeoIP(ctx, scheduled, policy, nil)
}
