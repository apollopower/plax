// Network management for per-instance service isolation.
package docker

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/errdefs"
)

func (d *Driver) CreateNetwork(ctx context.Context, name string) error {
	_, err := d.cli.NetworkCreate(ctx, name, network.CreateOptions{
		Driver: "bridge",
	})
	if err != nil {
		if errdefs.IsConflict(err) {
			return nil
		}
		return fmt.Errorf("docker: create network %s: %w", name, err)
	}
	return nil
}

func (d *Driver) RemoveNetwork(ctx context.Context, name string) error {
	if err := d.cli.NetworkRemove(ctx, name); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("docker: remove network %s: %w", name, err)
	}
	return nil
}
