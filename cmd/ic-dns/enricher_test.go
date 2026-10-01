package main

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/cmd/ic-dns/dns"
	"github.com/lxc/incus-compose/ievent/enricher"
	"github.com/lxc/incus-compose/ievent/iutil"
	"github.com/lxc/incus-compose/ievent/source"
	"github.com/lxc/incus-compose/internal/testlib"
	"github.com/lxc/incus-compose/shared"
)

// collectorPlugin collects events leaving the enricher.
type collectorPlugin struct {
	events chan *iutil.Event
}

func (c *collectorPlugin) Name() string                  { return "collector" }
func (c *collectorPlugin) Wants() []iutil.Want           { return nil }
func (c *collectorPlugin) Setup(_ iutil.SetupArgs) error { return nil }
func (c *collectorPlugin) Handle(ev *iutil.Event) {
	c.events <- ev
}

func TestEnricherReturnsIPOnInstanceStarted(t *testing.T) {
	testlib.SkipLocal(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	gc, err := client.NewTestClient(ctx)
	require.NoError(t, err)

	projectName := "test-dns-ip-" + strings.ToLower(shared.RandString(6))
	c, err := gc.EnsureProject(projectName, client.EnsureProjectWithCreate())
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = gc.DeleteProject(projectName, true)
	})

	conn, err := c.Connection()
	require.NoError(t, err)

	collector := &collectorPlugin{events: make(chan *iutil.Event, 64)}

	// Setup pipeline: source -> enricher -> dns -> collector
	dnsPlugin := dns.New(slog.Default(), dns.Suffix("incus"))
	enr := enricher.New(slog.Default(),
		enricher.Workers(4),
		enricher.ReadTimeout(10*time.Second),
	)

	plugins := []iutil.Plugin{enr, dnsPlugin, collector}
	src, err := source.New(slog.Default(), conn, plugins)
	require.NoError(t, err)

	srcCtx, cancelSrc := context.WithCancel(ctx)
	t.Cleanup(cancelSrc)

	go func() { _ = src.Run(srcCtx) }()
	go func() { _ = enr.Run(srcCtx) }()
	go func() { _ = dnsPlugin.Run(srcCtx) }()

	// Create network and container with an attached NIC.
	networkRes, err := c.Resource(client.KindNetwork, "default", &client.NetworkConfig{})
	require.NoError(t, err)

	imageRes, err := c.Resource(client.KindImage, "ghcr.io/ghcr-library/busybox:glibc", &client.ImageConfig{})
	require.NoError(t, err)
	image, ok := imageRes.(*client.Image)
	require.True(t, ok)

	instRes, err := c.Resource(client.KindInstance, "web", &client.InstanceConfig{
		Image: image.Name(),
		Entrypoint: []string{
			"sh", "-c",
			"echo \"[ \\$1 = bound ] && ip addr add \\$ip/\\$mask dev \\$interface\" > /tmp/dhcp.sh && chmod +x /tmp/dhcp.sh && sleep 2 && udhcpc -i eth0 -n -q -s /tmp/dhcp.sh && sleep 3600",
		},
		// Incus 7.3+ configures an OCI start hook (lxc.hook.start-host) running
		// forknet dhcp which waits up to 5 seconds for DHCP leases before starting
		// the container process. Blanking the hook bypasses this wait so we can
		// delay DHCP inside the container with udhcpc to simulate a DHCP race.
		Extensions: map[string]string{
			"raw.lxc": "lxc.hook.start-host=",
		},
		Devices: []client.InstanceDevice{
			{
				Name: "eth0",
				Config: client.InstanceDeviceConfig{
					DeviceType: client.InstanceDeviceTypeNic,
					Network:    networkRes,
				},
			},
		},
	})
	require.NoError(t, err)
	inst, ok := instRes.(*client.Instance)
	require.True(t, ok)

	stack := client.NewStack(c)
	stack.Add(networkRes, image, inst)
	require.NoError(t, stack.ForAction(client.ActionEnsure).Run(ctx, client.ActionEnsure, client.OptionCreate()))
	require.NoError(t, client.RunAction(ctx, inst, client.ActionStart, client.OptionNoHealthd()))

	// Wait for the instance-started event from enricher.
	var startedEvent *iutil.Event
	timeout := time.After(20 * time.Second)
	for startedEvent == nil {
		select {
		case ev := <-collector.events:
			if ev.Action() == incusapi.EventLifecycleInstanceStarted && ev.Name() == "web" {
				startedEvent = ev
			}
		case <-timeout:
			t.Fatal("timed out waiting for instance-started event")
		}
	}

	require.True(t, startedEvent.Enriched(iutil.EnrichedInstanceWithInterfaces),
		"instance-started event must be enriched with interfaces")

	interfaces := slices.Collect(startedEvent.Instance().Interfaces())
	require.NotEmpty(t, interfaces, "expected at least one interface")
	for _, iface := range interfaces {
		t.Logf("iface: %s/%s ipv4=%v ipv6=%v", iface.Project(), iface.Network(), iface.IPv4(), iface.IPv6())
	}

	assert.NotEmpty(t, interfaces[0].IPv4(),
		"enricher returned instance-started with EnrichedInstanceWithInterfaces but no IPv4 address")
}
