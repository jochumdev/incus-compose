package dns

import (
	"strings"

	"github.com/lxc/incus-compose/ievent/shared"
)

// labelPrefix is the namespace an instance or a project configures us from. The
// enricher hands configuration over whole; picking our keys out of it is ours.
const labelPrefix = "user.label.coredns."

// The keys, without the prefix.
const (
	metaZone     = "zone"
	metaService  = "service"
	metaAliases  = "aliases"
	metaTransfer = "transfer"
)

// LabelServiceCompose is what incus-compose stamps a service with. It wins over
// our own key, so a compose fleet is named by the compose file that owns it.
const LabelServiceCompose = "user.label.incus-compose.service"

// labels collects our keys with the prefix stripped. An empty value is dropped,
// which is how a value inherited from a profile is turned off again.
func labels(config map[string]string) map[string]string {
	var out map[string]string

	for key, value := range config {
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
//
// Transfer is dropped: it says a zone may be handed over whole, and a zone
// belongs to its project. Leaving it readable here would let one instance
// expose every sibling in the project.
func instanceLabels(ev *shared.Event) map[string]string {
	config := ev.Metadatas()

	out := labels(config)

	delete(out, metaTransfer)

	compose := strings.TrimSpace(config[LabelServiceCompose])
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
// not inherit: one name claimed by every instance is a collision, not a setting.
func projectLabels(ev *shared.Event) map[string]string {
	out := labels(ev.ProjectMetadatas())

	delete(out, metaAliases)

	return out
}
