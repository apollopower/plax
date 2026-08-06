package docker

import (
	"context"
	"fmt"
	"strings"

	"github.com/docker/docker/api/types/network"
)

func (d *Driver) CreateNetwork(ctx context.Context, name string) error {
	_, err := d.cli.NetworkCreate(ctx, name, network.CreateOptions{
		Driver: "bridge",
	})
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return nil
		}
		return fmt.Errorf("docker: create network %s: %w", name, err)
	}
	return nil
}

func (d *Driver) RemoveNetwork(ctx context.Context, name string) error {
	if err := d.cli.NetworkRemove(ctx, name); err != nil {
		if strings.Contains(err.Error(), "No such network") || strings.Contains(err.Error(), "not found") {
			return nil
		}
		return fmt.Errorf("docker: remove network %s: %w", name, err)
	}
	return nil
}
