package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"

	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/iclient"
	"github.com/lxc/incus-compose/internal/testlib"
	"github.com/lxc/incus-compose/shared"
)

// testImage is warm in the test cache and carries the busybox tools the health
// checks use: true, false, wget and httpd.
const testImage = "docker.io/library/busybox:glibc"

// testProject creates a throwaway Incus project, deleted on teardown. Each test
// gets its own, which is also how the daemon is used.
func testProject(t *testing.T, prefix string) *client.Client {
	t.Helper()

	// Not t.Context(): it is canceled before t.Cleanup runs, so the teardown delete would never reach Incus.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	gc, err := client.NewTestClient(ctx)
	require.NoError(t, err)
	require.NoError(t, gc.Connect())

	name := prefix + strings.ToLower(RandString(12))

	c, err := gc.EnsureProject(name,
		client.EnsureProjectWithCreate(),
		client.EnsureProjectWithSkipHealthd(),
	)
	require.NoError(t, err)
	require.NoError(t, c.Open())

	// The client and Incus log to the shared stderr, so name the project here.
	fmt.Fprintf(t.Output(), "incus project: %s\n", name)

	t.Cleanup(func() {
		err := c.Done()
		if err != nil {
			t.Errorf("closing client for project %s: %v", name, err)
		}

		err = gc.DeleteProject(name, true)
		if err != nil {
			t.Errorf("deleting project %s: %v", name, err)
		}
	})

	return c
}

// testContainer creates a busybox container carrying keys and returns its Incus
// name. It idles rather than exiting, so a check has something to exec into.
func testContainer(t *testing.T, c *client.Client, name string, keys map[string]string, start bool) string {
	t.Helper()

	ctx := t.Context()

	imgRes, err := c.Resource(client.KindImage, testImage, &client.ImageConfig{})
	require.NoError(t, err)

	img, ok := imgRes.(*client.Image)
	require.True(t, ok)

	instRes, err := c.Resource(client.KindInstance, name, &client.InstanceConfig{
		Image:      img.Name(),
		Extensions: keys,
		Entrypoint: []string{"/bin/sh"},
	})
	require.NoError(t, err)

	inst, ok := instRes.(*client.Instance)
	require.True(t, ok)

	// NoHealthd throughout, or user.healthcheck.stopped makes the instance
	// look deliberately stopped to the daemon under test.
	stack := client.NewStack(c)
	stack.Add(img, inst)
	require.NoError(t, stack.ForAction(client.ActionEnsure).
		Run(ctx, client.ActionEnsure, client.OptionCreate(), client.OptionNoHealthd()))

	if start {
		require.NoError(t, client.RunAction(ctx, inst,
			client.ActionStart, client.OptionNoHealthd()))
	}

	return inst.IncusName()
}

// testConn returns the project-scoped Incus connection the actions take.
func testConn(t *testing.T, c *client.Client) *iclient.Connection {
	t.Helper()

	return dialTestRemote(t).WithProject(c.Project())
}

// dialTestRemote dials the remote the test environment points at.
func dialTestRemote(t *testing.T) *iclient.Connection {
	t.Helper()

	config, err := iclient.ReadConfig("")
	require.NoError(t, err)

	info, err := config.RemoteInfos(os.Getenv("INCUS_REMOTE"))
	require.NoError(t, err)

	conn, err := iclient.NewConnection(info)
	require.NoError(t, err)

	return conn
}

// refusedConn returns a real client whose every call fails immediately, for the
// state machine tests: they never want an answer, only somewhere to send.
func refusedConn(t *testing.T) *iclient.Connection {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	conn, err := iclient.NewConnection(&iclient.ConfigRemoteInfo{
		Name:               "refused",
		Addrs:              []string{"https://" + addr},
		Protocol:           "incus",
		InsecureSkipVerify: true,
	})
	require.NoError(t, err)

	return conn
}

// testInstance builds the Incus instance parseInstanceConfig reads, with the
// healthcheck keys merged over a minimal valid base.
func testInstance(name string, running bool, config map[string]string) *incusApi.Instance {
	status := incusApi.Stopped
	if running {
		status = incusApi.Running
	}

	cfg := incusApi.ConfigMap{}
	for k, v := range config {
		cfg[k] = v
	}

	return &incusApi.Instance{
		Name:       name,
		StatusCode: status,
		Status:     status.String(),
		InstancePut: incusApi.InstancePut{
			Config: cfg,
		},
	}
}

// healthKeys is shorthand for the user.healthcheck.* map a test wants, carrying
// the opt-in. Tests about its absence set the keys by hand.
func healthKeys(pairs map[string]string) map[string]string {
	out := map[string]string{
		shared.HealthEnabledKey: "true",
	}

	for k, v := range pairs {
		out[shared.HealthKeyPrefix+k] = v
	}

	return out
}

func TestMain(m *testing.M) {
	os.Exit(testlib.Main(m))
}
