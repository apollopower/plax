package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

type ServiceConfig struct {
	InstanceName string            `json:"instance_name"`
	ServiceName  string            `json:"service_name"`
	Image        string            `json:"image"`
	Command      []string          `json:"command,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	PortMap      map[string]int    `json:"port_map,omitempty"`
	Volumes      []string          `json:"volumes,omitempty"`
	NetworkName  string            `json:"network_name"`
}

type Driver struct {
	cli *client.Client
}

func NewDriver() (*Driver, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker: cannot connect to daemon. Is Docker running?")
	}
	return &Driver{cli: cli}, nil
}

func (d *Driver) Close() error {
	return d.cli.Close()
}

func (d *Driver) RunService(ctx context.Context, cfg ServiceConfig) (string, error) {
	containerName := sanitizeName("plax-" + cfg.InstanceName + "-" + cfg.ServiceName)

	if err := d.pullImage(ctx, cfg.Image); err != nil {
		return "", err
	}

	for _, vol := range cfg.Volumes {
		volName := sanitizeName("plax-" + cfg.InstanceName + "-" + cfg.ServiceName + "-" + vol)
		if err := d.ensureVolume(ctx, volName); err != nil {
			return "", err
		}
	}

	portSet, portBindings, err := buildPortBindings(cfg.PortMap)
	if err != nil {
		return "", err
	}

	envVars := []string{}
	for k, v := range cfg.Env {
		envVars = append(envVars, k+"="+v)
	}

	volumeBinds := []string{}
	for _, vol := range cfg.Volumes {
		volName := sanitizeName("plax-" + cfg.InstanceName + "-" + cfg.ServiceName + "-" + vol)
		volumeBinds = append(volumeBinds, volName+":/data")
	}

	containerCfg := &container.Config{
		Image:        cfg.Image,
		Cmd:          cfg.Command,
		Env:          envVars,
		ExposedPorts: portSet,
	}

	hostCfg := &container.HostConfig{
		PortBindings:  portBindings,
		NetworkMode:   container.NetworkMode(cfg.NetworkName),
		RestartPolicy: container.RestartPolicy{Name: "no"},
		Binds:         volumeBinds,
	}

	resp, err := d.cli.ContainerCreate(ctx, containerCfg, hostCfg, nil, nil, containerName)
	if err != nil {
		if strings.Contains(err.Error(), "Conflict") {
			_ = d.cli.ContainerRemove(ctx, containerName, container.RemoveOptions{Force: true})
			resp, err = d.cli.ContainerCreate(ctx, containerCfg, hostCfg, nil, nil, containerName)
			if err != nil {
				return "", fmt.Errorf("docker: create container %s: %w", containerName, err)
			}
		} else {
			return "", fmt.Errorf("docker: create container %s: %w", containerName, err)
		}
	}

	if err := d.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("docker: start container %s: %w", containerName, err)
	}

	return resp.ID, nil
}

func (d *Driver) StopService(ctx context.Context, containerID string) error {
	timeout := 10
	if err := d.cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout}); err != nil {
		if strings.Contains(err.Error(), "No such container") {
			return nil
		}
		return fmt.Errorf("docker: stop container: %w", err)
	}
	return nil
}

func (d *Driver) RemoveService(ctx context.Context, containerID string) error {
	if err := d.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); err != nil {
		if strings.Contains(err.Error(), "No such container") {
			return nil
		}
		return fmt.Errorf("docker: remove container: %w", err)
	}
	return nil
}

func (d *Driver) RemoveVolume(ctx context.Context, volumeName string) error {
	if err := d.cli.VolumeRemove(ctx, volumeName, true); err != nil {
		if strings.Contains(err.Error(), "No such volume") {
			return nil
		}
		return fmt.Errorf("docker: remove volume: %w", err)
	}
	return nil
}

func (d *Driver) pullImage(ctx context.Context, imageName string) error {
	// Pull only when absent: unconditional pulls break offline use and add
	// latency to every start.
	if _, _, err := d.cli.ImageInspectWithRaw(ctx, imageName); err == nil {
		return nil
	}

	rd, err := d.cli.ImagePull(ctx, imageName, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("docker: pull %s: %w", imageName, err)
	}
	defer func() { _ = rd.Close() }()

	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		_, err := rd.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("docker: pull %s: read progress: %w", imageName, err)
		}
	}

	return nil
}

func (d *Driver) ensureVolume(ctx context.Context, name string) error {
	_, err := d.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name})
	if err != nil {
		return fmt.Errorf("docker: create volume %s: %w", name, err)
	}
	return nil
}

func buildPortBindings(portMap map[string]int) (nat.PortSet, nat.PortMap, error) {
	portSet := nat.PortSet{}
	portMapOut := nat.PortMap{}

	for containerPort, hostPort := range portMap {
		np, err := nat.NewPort("tcp", containerPort)
		if err != nil {
			return nil, nil, fmt.Errorf("docker: invalid port %s: %w", containerPort, err)
		}
		portSet[np] = struct{}{}
		portMapOut[np] = []nat.PortBinding{
			{HostIP: "127.0.0.1", HostPort: strconv.Itoa(hostPort)},
		}
	}

	return portSet, portMapOut, nil
}

func sanitizeName(name string) string {
	n := strings.ToLower(name)
	n = strings.ReplaceAll(n, "_", "-")
	return n
}
