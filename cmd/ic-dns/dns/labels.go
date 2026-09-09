package dns

import (
	"strings"

	"github.com/lxc/incus-compose/ievent/iutil"
)

// labelPrefix is the namespace an instance or a project configures us from. The
// enricher hands configuration over whole; picking our keys out of it is ours.
const labelPrefix = "dns."

// userLabelPrefix is where Incus keeps an instance's own labels. A project has
// no such prefix: its configuration is what `incus project set` wrote, and that
// is already the project's own namespace.
const userLabelPrefix = "user.label."

// labelServiceCompose is what incus-compose stamps a service with. It wins over
// our own key, so a compose fleet is named by the compose file that owns it.
const labelServiceCompose = "incus-compose.service"

// The keys, without the prefix.
const (
	metaZone     = "zone"
	metaService  = "service"
	metaAliases  = "aliases"
	metaNS       = "ns"
	metaTransfer = "transfer"
)

// labelKind says which side of a read may set a key. A key nothing declares may
// come from either side, and the project wins.
type labelKind int

const (
	// labelProject is the project's to set, so an instance naming it is ignored.
	labelProject labelKind = iota + 1

	// labelInstance is the instance's own, so a project naming it reaches nothing.
	labelInstance
)

// kinds is what each of our keys may be set by. Keyed the way the merge sees
// them, which is with our prefix still on.
var kinds = map[string]labelKind{
	// A name server is the zone's, not one instance's to claim.
	labelPrefix + metaNS: labelProject,

	// Transfer opts a zone in, so it is the zone's to say. Two projects on one
	// zone name union, as their name servers do: either one opts it in.
	labelPrefix + metaTransfer: labelProject,

	// An alias every instance in a project claimed would be contested by all of
	// them and answered for none.
	labelPrefix + metaAliases: labelInstance,

	// A service is what one instance's replicas share, so it is set per
	// instance. A project naming one would put every instance it holds under a
	// single record, replicas of each other or not.
	labelPrefix + metaService: labelInstance,
	labelServiceCompose:       labelInstance,

	// Zone is left either side's on purpose: a project names the zone its fleet
	// publishes under, and an instance may name one to join a zone another
	// project already serves. Where both name one, the project wins.
}

// merged folds what the project sets over what the instance sets, by what each
// key may come from. The enricher hands both over untouched; deciding between
// them is ours, because what a key means is ours.
func merged(ev *iutil.Event) map[string]string {
	out := map[string]string{}

	for key, value := range ev.Instance().Config() {
		key, ours := strings.CutPrefix(key, userLabelPrefix)
		if !ours || kinds[key] == labelProject {
			continue
		}

		out[key] = value
	}

	for key, value := range ev.Project().Config() {
		if kinds[key] == labelInstance {
			continue
		}

		out[key] = value
	}

	return out
}

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

// meta reads our keys off one event, from both sides of the read. The compose
// service is applied here because what a key means is the consumer's.
func meta(ev *iutil.Event) map[string]string {
	config := merged(ev)

	out := labels(config)

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
