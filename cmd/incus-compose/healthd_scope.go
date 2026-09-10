package main

import (
	"fmt"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/project"
	"github.com/lxc/incus-compose/shared"
)

// resolveHealthdScope returns the scope for the project, first match wins, so a
// project keeps the scope it carries until that key itself is changed.
func resolveHealthdScope(projectConfig map[string]string, cliScope, composeScope string) string {
	sources := []struct {
		where string
		value string
	}{
		{"the Incus project's " + shared.HealthScopeKey, projectConfig[shared.HealthScopeKey]},
		{"--healthd-scope", cliScope},
		{"x-incus-compose.healthd.scope", composeScope},
	}

	for _, source := range sources {
		if source.value == "" {
			continue
		}

		return source.value
	}

	return shared.HealthScopeGlobal
}

// healthdClient returns the scope p carries and the client of the project its
// daemon lives in. Only the stored scope counts: the flag and the compose file
// say what the next `up` will do, not what watches p now.
func healthdClient(p *project.Project, c *client.Client) (*client.Client, string, error) {
	projectConfig, err := c.Global().ProjectConfig(p.Name)
	if err != nil {
		return nil, "", err
	}

	scope := projectConfig[shared.HealthScopeKey]
	if scope != shared.HealthScopeGlobal {
		return c, scope, nil
	}

	hc, err := c.Global().EnsureProject(systemProject)
	if err != nil {
		return nil, "", fmt.Errorf("getting the %s project: %w", systemProject, err)
	}

	return hc, scope, nil
}
