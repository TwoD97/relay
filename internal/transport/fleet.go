package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/TwoD97/relay/internal/fleetregistry"
)

// PublishFleet sends discovery metadata over this existing authenticated SSH
// link. It never copies SSH configuration, keys, agents, or provider credentials.
func (c *Connection) PublishFleet(ctx context.Context, snapshot fleetregistry.Snapshot) error {
	if err := fleetregistry.Validate(snapshot); err != nil {
		return err
	}
	data, err := json.Marshal(snapshot)
	if err != nil || len(data) > fleetregistry.MaxBytes {
		return errors.New("fleet snapshot exceeds its size limit")
	}
	_, err = c.Run(ctx, `exec "$HOME/.local/share/relay/bin/relay" fleet import`, strings.NewReader(string(data)))
	if err != nil {
		return fmt.Errorf("publish Relay fleet discovery: %w", err)
	}
	return nil
}

func fleetEndpoint(user, host string, port int) (string, int, error) {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	target := user + "@" + host
	if err := ValidateTarget(target); err != nil || user == "" || port < 1 || port > 65535 {
		return "", 0, errors.New("SSH configuration did not provide a portable user, hostname and port")
	}
	return target, port, nil
}
