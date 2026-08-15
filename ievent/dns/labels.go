package dns

import (
	"strings"

	"github.com/lxc/incus-compose/ievent/iutil"
)

// labelPrefix is the namespace an instance or a project configures us from. The
// enricher hands configuration over whole; picking our keys out of it is ours.
const labelPrefix = "coredns."

// labelServiceCompose is what incus-compose stamps a service with. It wins over
// our own key, so a compose fleet is named by the compose file that owns it.
const labelServiceCompose = "incus-compose.service"

// The keys, without the prefix.
const (
	metaZone     = "zone"
	metaService  = "service"
	metaAliases  = "aliases"
	metaTransfer = "transfer"
)

// labels collects our keys with the prefix stripped. An empty value is dropped,
// which is how a value inherited from a profile is turned off again.
func labels(labels map[string]string) map[string]string {
	var out map[string]string

	for key, value := range labels {
		if !strings.HasPrefix(key, labelPrefix) {
			continue
		}

		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}

		if out == nil {
			out = map[string]string{}
		}

		out[strings.TrimPrefix(key, labelPrefix)] = value
	}

	return out
}

// instanceLabels reads our keys off one event's instance configuration. The
// compose service is applied here because what a key means is the consumer's.
func instanceLabels(ev *iutil.Event) map[string]string {
	config := ev.InstanceLabels()

	out := labels(config)

	// Transfer is dropped: it says a zone may be handed over whole, and a zone
	// belongs to its project.
	delete(out, metaTransfer)

	compose := strings.TrimSpace(config[labelServiceCompose])
	if compose == "" {
		return out
	}

	if out == nil {
		out = map[string]string{}
	}

	out[metaService] = compose

	return out
}

// projectLabels reads our keys off the project's own configuration. Aliases do
// not inherit.
func projectLabels(ev *iutil.Event) map[string]string {
	out := labels(ev.ProjectLabels())

	delete(out, metaAliases)

	return out
}
