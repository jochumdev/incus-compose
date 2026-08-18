package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/internal/testlib"
	"github.com/lxc/incus-compose/shared"
)

// portDevices is what `up` leaves on an instance publishing 8080:80,
// 8443:443/udp and 9090:80, next to devices port has no business matching.
var portDevices = map[string]map[string]string{
	"eth0": {
		"type":    "nic",
		"network": "portproject-default",
	},
	"proxy-8080": {
		"type":    "proxy",
		"listen":  "tcp:0.0.0.0:8080",
		"connect": "tcp:127.0.0.1:80",
	},
	"proxy-8081": {
		"type":    "proxy",
		"listen":  "tcp:0.0.0.0:8081",
		"connect": "tcp:0.0.0.0:80",
		"nat":     "true",
	},
	"proxy-8443": {
		"type":    "proxy",
		"listen":  "udp:127.0.0.1:8443",
		"connect": "udp:127.0.0.1:443",
	},
	"proxy-8666": {
		"type":    "proxy",
		"listen":  "tcp:[fd42::1]:8666",
		"connect": "tcp:[::1]:666",
	},
}

func TestPublishedPort(t *testing.T) {
	tests := []struct {
		name     string
		protocol string
		port     string
		want     string
	}{
		{name: "two devices publish 80, the lowest device name wins", protocol: "tcp", port: "80", want: "0.0.0.0:8080"},
		{name: "udp is a port of its own", protocol: "udp", port: "443", want: "127.0.0.1:8443"},
		{name: "an IPv6 binding keeps its brackets", protocol: "tcp", port: "666", want: "[fd42::1]:8666"},
		{name: "the protocol has to match", protocol: "udp", port: "80"},
		{name: "an unpublished port is not found", protocol: "tcp", port: "8080"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listen, ok := publishedPort(portDevices, tt.protocol, tt.port)
			assert.Equal(t, tt.want != "", ok)
			assert.Equal(t, tt.want, listen)
		})
	}
}

func TestPublishedPortNoDevices(t *testing.T) {
	listen, ok := publishedPort(nil, "tcp", "80")
	assert.False(t, ok)
	assert.Empty(t, listen)
	assert.Empty(t, proxyPorts(nil))
}

func TestProxyPorts(t *testing.T) {
	assert.Equal(t, []string{"80/tcp", "80/tcp", "443/udp", "666/tcp"}, proxyPorts(portDevices))
}

// TestE2EPort asks for the host side of a plain and of a NAT proxy, then for a
// port that is not published. The ports are the test's own: `with-ports` binds
// 8080/8081 on the host and TestE2ENATProxy runs in parallel with this one.
func TestE2EPort(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	testlib.SkipE2E(t)

	ctx := t.Context()
	pn := t.Name()

	dir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `services:
  web:
    image: docker.io/alpine:edge
    ports:
      - "18080:80"
      - "18053:53/udp"

  web-nat:
    image: docker.io/alpine:edge
    ports:
      - published: "18081"
        target: "80"
        x-incus-compose:
          nat: true
`,
	})
	compose := filepath.Join(dir, "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	tests := []struct {
		name    string
		flags   []string
		service string
		port    string
		want    string
	}{
		{name: "a published port", service: "web", port: "80", want: "0.0.0.0:18080"},
		{name: "a nat published port", service: "web-nat", port: "80", want: "0.0.0.0:18081"},
		{name: "udp is a port of its own", flags: []string{"--protocol", "udp"}, service: "web", port: "53", want: "0.0.0.0:18053"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"-f", compose, "port"}, tt.flags...)
			args = append(args, tt.service, tt.port)

			stdout, err := testlib.RunCompose(ctx, t, pn, "", nil, args...)
			require.NoError(t, err)
			assert.Equal(t, tt.want, strings.TrimSpace(stdout))
		})
	}

	t.Run("the tcp port is not the udp one", func(t *testing.T) {
		_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "port", "web", "53")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "53/udp", "the error names the ports the instance does have")
	})

	t.Run("a stopped instance answers too", func(t *testing.T) {
		_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "stop", "web")
		require.NoError(t, err)

		stdout, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "port", "web", "80")
		require.NoError(t, err)
		assert.Equal(t, "0.0.0.0:18080", strings.TrimSpace(stdout))
	})
}

// TestE2EPortForward checks the incus command line port-forward hands off,
// which is all of it that is ours - the rest is `incus port-forward`. The
// service publishes nothing, which is the case port-forward exists for.
func TestE2EPortForward(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	testlib.SkipE2E(t)

	ctx := t.Context()
	pn := t.Name()

	gc, err := client.NewTestClient(t.Context())
	require.NoError(t, err)
	c, err := gc.EnsureProject("default")
	require.NoError(t, err)

	if !c.Global().HasExtension(shared.Incus73Extension) {
		t.Skip("port-forward tests require at least incus 7.3 or 7.0.2 LTS")
	}

	dir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `services:
  web:
    image: docker.io/alpine:edge
`,
	})
	compose := filepath.Join(dir, "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "a listen port of its own", args: []string{"80", "18080"}, want: "port-forward web-1 80 18080"},
		{name: "no listen port listens on the target port", args: []string{"80"}, want: "port-forward web-1 80 80"},
		{name: "an address goes through verbatim", args: []string{"0.0.0.0:80", "[::1]:18080"}, want: "port-forward web-1 0.0.0.0:80 [::1]:18080"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"-f", compose, "port-forward", "--dry-run", "web"}, tt.args...)

			stdout, err := testlib.RunCompose(ctx, t, pn, "", nil, args...)
			require.NoError(t, err)

			execPath, cmdLine, ok := strings.Cut(strings.TrimSpace(stdout), " ")
			require.True(t, ok, "unexpected output: %q", stdout)
			assert.Equal(t, "incus", filepath.Base(execPath))
			assert.Equal(t, tt.want, cmdLine)
		})
	}
}
