package docker

import (
	"context"
	"os"
	"testing"
)

func testDriver(t *testing.T) *Driver {
	t.Helper()

	if _, err := os.Stat("/var/run/docker.sock"); err != nil && os.Getenv("DOCKER_HOST") == "" {
		t.Skip("skipping: Docker daemon not available")
	}

	d, err := NewDriver()
	if err != nil {
		t.Skipf("skipping: cannot connect to Docker: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestRunService_Success(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()

	id, err := d.RunService(ctx, ServiceConfig{
		InstanceName: "dockertest",
		ServiceName:  "nginx",
		Image:        "nginx:alpine",
		NetworkName:  "bridge",
	})
	if err != nil {
		t.Fatalf("RunService: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty container ID")
	}

	if err := d.StopService(ctx, id); err != nil {
		t.Errorf("StopService: %v", err)
	}
	if err := d.RemoveService(ctx, id); err != nil {
		t.Errorf("RemoveService: %v", err)
	}
}

func TestRunService_PortBinding(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()

	id, err := d.RunService(ctx, ServiceConfig{
		InstanceName: "porttest",
		ServiceName:  "nginx",
		Image:        "nginx:alpine",
		NetworkName:  "bridge",
		PortMap:      map[string]int{"80": 18080},
	})
	if err != nil {
		t.Fatalf("RunService: %v", err)
	}

	t.Cleanup(func() {
		_ = d.StopService(context.Background(), id)
		_ = d.RemoveService(context.Background(), id)
	})
}

func TestStopService_NotRunning(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()

	err := d.StopService(ctx, "nonexistent-container-id")
	if err != nil {
		t.Errorf("StopService on nonexistent should be no-op: %v", err)
	}
}

func TestRemoveService_Success(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()

	id, err := d.RunService(ctx, ServiceConfig{
		InstanceName: "removetest",
		ServiceName:  "nginx",
		Image:        "nginx:alpine",
		NetworkName:  "bridge",
	})
	if err != nil {
		t.Fatalf("RunService: %v", err)
	}

	_ = d.StopService(ctx, id)
	if err := d.RemoveService(ctx, id); err != nil {
		t.Errorf("RemoveService: %v", err)
	}
}

func TestRemoveVolume_NotExists(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()

	err := d.RemoveVolume(ctx, "nonexistent-volume-name")
	if err != nil {
		t.Errorf("RemoveVolume on nonexistent should be no-op: %v", err)
	}
}

func TestCreateNetwork_Idempotent(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()

	name := "plax-test-network"
	_ = d.RemoveNetwork(ctx, name)

	if err := d.CreateNetwork(ctx, name); err != nil {
		t.Fatalf("first CreateNetwork: %v", err)
	}

	if err := d.CreateNetwork(ctx, name); err != nil {
		t.Fatalf("second CreateNetwork should be idempotent: %v", err)
	}

	if err := d.RemoveNetwork(ctx, name); err != nil {
		t.Errorf("RemoveNetwork: %v", err)
	}
}

func TestRemoveNetwork_Success(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()

	name := "plax-test-network-remove"
	_ = d.RemoveNetwork(ctx, name)

	if err := d.CreateNetwork(ctx, name); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}

	if err := d.RemoveNetwork(ctx, name); err != nil {
		t.Errorf("RemoveNetwork: %v", err)
	}
}
