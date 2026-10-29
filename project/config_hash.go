package project

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"

	"github.com/compose-spec/compose-go/v2/types"
	"go.yaml.in/yaml/v4"
)

// ServiceConfigHash returns the hash of what a service's instance is created from.
func ServiceConfigHash(service types.ServiceConfig) (string, error) {
	// Changing these does not change the instance itself.
	service.Build = nil
	service.PullPolicy = ""
	service.Scale = nil
	service.DependsOn = nil
	service.Profiles = nil

	// The pointer is shared with the project's service, so clear a copy.
	if service.Deploy != nil {
		deploy := *service.Deploy
		deploy.Replicas = nil
		deploy.UpdateConfig = nil
		deploy.RollbackConfig = nil
		service.Deploy = &deploy

		if reflect.ValueOf(deploy).IsZero() {
			service.Deploy = nil
		}
	}

	// YAML, not JSON: compose-go tags Extensions json:"-", which would drop
	// every x-incus and x-incus-compose block.
	out, err := yaml.Marshal(service)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(out)

	// Bump the prefix only when the canonical form changes.
	return "v1:" + hex.EncodeToString(sum[:]), nil
}
