package main

import (
	"encoding/json"
	"testing"

	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
)

func TestDNSIPAddresses_FromState(t *testing.T) {
	t.Parallel()

	state := &client.InstanceState{
		IncusInstanceState: &incusApi.InstanceState{
			Network: map[string]incusApi.InstanceStateNetwork{
				"lo": {
					Type: "loopback",
					Addresses: []incusApi.InstanceStateNetworkAddress{
						{Family: "inet", Address: "127.0.0.1", Scope: "local"},
					},
				},
				"eth0": {
					Type: "broadcast",
					Addresses: []incusApi.InstanceStateNetworkAddress{
						{Family: "inet", Address: "10.0.0.53", Scope: "global"},
						{Family: "inet6", Address: "fd00::53", Scope: "global"},
						{Family: "inet6", Address: "fe80::1", Scope: "link"},
					},
				},
			},
		},
	}

	ipv4, ipv6 := dnsIPAddresses(state)
	assert.Equal(t, "10.0.0.53", ipv4)
	assert.Equal(t, "fd00::53", ipv6)
}

func TestDNSIPAddresses_FromDevicesFallback(t *testing.T) {
	t.Parallel()

	state := &client.InstanceState{
		IncusInstance: &incusApi.Instance{
			Devices: map[string]map[string]string{
				"eth0": {
					"type":         "nic",
					"ipv4.address": "10.100.0.53/24",
					"ipv6.address": "fd42::53/64",
				},
			},
		},
	}

	ipv4, ipv6 := dnsIPAddresses(state)
	assert.Equal(t, "10.100.0.53", ipv4)
	assert.Equal(t, "fd42::53", ipv6)
}

func TestDNSIPAddresses_Empty(t *testing.T) {
	t.Parallel()

	state := &client.InstanceState{}
	ipv4, ipv6 := dnsIPAddresses(state)
	assert.Empty(t, ipv4)
	assert.Empty(t, ipv6)
}

func TestDNSStatusCommand_Flags(t *testing.T) {
	t.Parallel()

	cmd := newDNSStatusCommand()
	assert.Equal(t, "status", cmd.Name)
	assert.NotEmpty(t, cmd.Flags)

	var portFlag *cli.IntFlag
	var formatFlag *cli.StringFlag
	var metricsFlag *cli.BoolFlag
	for _, f := range cmd.Flags {
		switch flag := f.(type) {
		case *cli.IntFlag:
			if flag.Name == "port" {
				portFlag = flag
			}
		case *cli.StringFlag:
			if flag.Name == "format" {
				formatFlag = flag
			}
		case *cli.BoolFlag:
			if flag.Name == "metrics" {
				metricsFlag = flag
			}
		}
	}
	assert.NotNil(t, metricsFlag, "expected --metrics flag to be present")
	if assert.NotNil(t, portFlag, "expected --port flag to be present") {
		assert.Equal(t, 9153, portFlag.Value)
	}
	if assert.NotNil(t, formatFlag, "expected --format flag to be present") {
		assert.Equal(t, "text", formatFlag.Value)
		assert.NoError(t, formatFlag.Action(t.Context(), cmd, "text"))
		assert.NoError(t, formatFlag.Action(t.Context(), cmd, "json"))
		assert.Error(t, formatFlag.Action(t.Context(), cmd, "yaml"))
		assert.Error(t, formatFlag.Action(t.Context(), cmd, "table"))
	}
}

func TestSidecarStatusReport_JSON(t *testing.T) {
	t.Parallel()

	report := sidecarStatusReport{
		Status:  "ready",
		IPv4:    "10.0.0.53",
		IPv6:    "fd00::53",
		Metrics: "some_metric 1\n",
	}

	data, err := json.Marshal(report)
	assert.NoError(t, err)
	assert.Contains(t, string(data), `"status":"ready"`)
	assert.Contains(t, string(data), `"ipv4":"10.0.0.53"`)
	assert.Contains(t, string(data), `"ipv6":"fd00::53"`)
	assert.Contains(t, string(data), `"metrics":"some_metric 1\n"`)

	reportNoMetrics := sidecarStatusReport{
		Status: "stopped",
		IPv4:   "",
		IPv6:   "",
	}
	dataNoMetrics, err := json.Marshal(reportNoMetrics)
	assert.NoError(t, err)
	assert.NotContains(t, string(dataNoMetrics), `"metrics"`)
	assert.Contains(t, string(dataNoMetrics), `"status":"stopped"`)
	assert.Contains(t, string(dataNoMetrics), `"ipv4":""`)
	assert.Contains(t, string(dataNoMetrics), `"ipv6":""`)
}
