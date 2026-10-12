//go:build windows || relay_ssh_native

package transport

import "context"

func (c *Connection) FleetEndpoint(ctx context.Context) (string, int, error) {
	resolved, err := resolveNativeConfig(ctx, c.cfg)
	if err != nil {
		return "", 0, err
	}
	return fleetEndpoint(resolved.user, resolved.host, resolved.port)
}
