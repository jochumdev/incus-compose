package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
)

type logResource struct {
	name     string
	priority int
}

func (r logResource) Kind() client.Kind { return client.KindInstance }
func (r logResource) Name() string      { return r.name }
func (r logResource) IncusName() string { return r.name }
func (r logResource) Priority() int     { return r.priority }
func (r logResource) IsEnsured() bool   { return false }
func (r logResource) Created() bool     { return false }

func TestContainerStatusesFormats(t *testing.T) {
	t.Parallel()

	status := ProjectStatus{
		Kind:      "container",
		Name:      "web",
		IncusName: "web-1",
		Image:     "docker.io/nginx:alpine",
		Status:    "Running",
		Addresses: []string{"10.0.0.2", "fd42::2"},
	}

	t.Run("table", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		statuses := NewContainerStatuses(&buf)
		statuses.Add(status)

		require.NoError(t, statuses.Table())
		assert.Contains(t, buf.String(), "KIND")
		assert.Contains(t, buf.String(), "web-1")
		assert.Contains(t, buf.String(), "10.0.0.2, fd42::2")
	})

	t.Run("json", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		statuses := NewContainerStatuses(&buf)
		statuses.Add(status)

		require.NoError(t, statuses.JSON())
		assert.Contains(t, buf.String(), `"name": "web"`)
		assert.Contains(t, buf.String(), `"addresses": [`)
	})

	t.Run("yaml", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		statuses := NewContainerStatuses(&buf)
		statuses.Add(status)

		require.NoError(t, statuses.Yaml())
		assert.Contains(t, buf.String(), "name: web")
		assert.Contains(t, buf.String(), "incus_name: web-1")
	})
}

func TestSortResources(t *testing.T) {
	t.Parallel()

	resources := []client.Resource{
		logResource{name: "web-2", priority: client.PriorityInstance},
		logResource{name: "ic-net", priority: client.PriorityNetwork},
		logResource{name: "db-1", priority: client.PriorityInstance},
		logResource{name: "web-1", priority: client.PriorityInstance},
	}

	names := []string{}
	for _, r := range sortResources(resources) {
		names = append(names, r.IncusName())
	}

	assert.Equal(t, []string{"ic-net", "db-1", "web-1", "web-2"}, names)
	assert.Equal(t, "web-2", resources[0].IncusName(), "input slice must stay untouched")
}

func TestLogFormatterNoColor(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	formatter := newLogFormatter(&buf, true)

	formatter.registerService("web")
	formatter.registerService("database")
	formatter.write(logResource{name: "web"}, []byte("first\npartial"))
	formatter.write(logResource{name: "database"}, []byte("ready\n"))
	formatter.flush()

	output := buf.String()
	assert.Contains(t, output, "web      | first\n")
	assert.Contains(t, output, "database | ready\n")
	assert.Contains(t, output, "web      | partial\n")
}

func TestLogFormatterColor(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	formatter := newLogFormatter(&buf, false)

	formatter.write(logResource{name: "web"}, []byte("hello\n"))
	formatter.flush()

	output := buf.String()
	assert.True(t, strings.Contains(output, "\033["))
	assert.Contains(t, output, "web | ")
	assert.Contains(t, output, "hello\n")
}
