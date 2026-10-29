package project

import (
	"fmt"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	incusApi "github.com/lxc/incus/v7/shared/api"

	"github.com/lxc/incus-compose/client"
)

// serviceAllowLocalStorage reports whether service sets x-incus-compose.allow-local-storage: true.
func serviceAllowLocalStorage(service types.ServiceConfig) bool {
	var raw map[string]any
	ok, err := service.Extensions.Get("x-incus-compose", &raw)
	if !ok || err != nil {
		return false
	}

	allow, _ := raw["allow-local-storage"].(bool)

	return allow
}

// serviceRestartPolicy returns the effective restart policy for a service,
// falling back to deploy.restart_policy.condition when service.Restart is empty.
func serviceRestartPolicy(service types.ServiceConfig) string {
	if service.Restart != "" {
		return service.Restart
	}

	if service.Deploy == nil || service.Deploy.RestartPolicy == nil {
		return ""
	}

	switch service.Deploy.RestartPolicy.Condition {
	case "none":
		return "no"
	case "on-failure":
		return "on-failure"
	case "any", "":
		return "always"
	default:
		return service.Deploy.RestartPolicy.Condition
	}
}

// clusterPlacementEnv holds the cluster state needed to resolve placement for a service.
type clusterPlacementEnv struct {
	clustered   bool
	serverName  string
	networkType string
	remotePool  bool
	members     []incusApi.ClusterMember
}

// resolveServicePlacement computes the desired replica count and per-replica
// cluster target member names for a service.
func (p *Project) resolveServicePlacement(c *client.Client, service types.ServiceConfig, scaleOverride *int) (int, []string, error) {
	var env clusterPlacementEnv
	if c != nil && c.Global().IsClustered() {
		members, err := c.Global().ClusterMembers()
		if err != nil {
			return 0, nil, fmt.Errorf("service %q: listing cluster members: %w", service.Name, err)
		}

		remotePool, err := p.isServiceStorageRemote(c, service)
		if err != nil {
			return 0, nil, fmt.Errorf("service %q: checking storage pool: %w", service.Name, err)
		}

		env = clusterPlacementEnv{
			clustered:   true,
			serverName:  c.Global().ServerName(),
			networkType: c.NetworkType(),
			remotePool:  remotePool,
			members:     members,
		}
	}

	desired, targets, warnLocal, err := resolveServicePlacementEnv(env, service, scaleOverride, p.ClientConfig.AllowLocalStorage)
	if err != nil {
		return 0, nil, err
	}

	if warnLocal && c != nil {
		c.LogWarn(
			"Deploying multiple replicas on a cluster with local storage; volumes and instances are member-local",
			"service", service.Name,
			"replicas", desired,
		)
	}

	return desired, targets, nil
}

func resolveServicePlacementEnv(env clusterPlacementEnv, service types.ServiceConfig, scaleOverride *int, projectAllowLocal bool) (int, []string, bool, error) {
	mode := "replicated"
	if service.Deploy != nil && service.Deploy.Mode != "" {
		mode = service.Deploy.Mode
	}

	switch mode {
	case "replicated", "global":
	default:
		return 0, nil, false, fmt.Errorf("service %q: unsupported deploy.mode %q (must be replicated or global)", service.Name, mode)
	}

	desired := 1
	if scaleOverride != nil {
		desired = *scaleOverride
	} else if service.Deploy != nil && service.Deploy.Replicas != nil {
		desired = *service.Deploy.Replicas
	}

	if !env.clustered {
		if mode == "global" && scaleOverride == nil {
			desired = 1
		}

		return desired, make([]string, desired), false, nil
	}

	var constraints []string
	var preferences []types.PlacementPreferences
	var maxReplicas uint64
	if service.Deploy != nil {
		constraints = service.Deploy.Placement.Constraints
		preferences = service.Deploy.Placement.Preferences
		maxReplicas = service.Deploy.Placement.MaxReplicas
	}

	candidates, err := filterClusterMembers(env.members, constraints)
	if err != nil {
		return 0, nil, false, fmt.Errorf("service %q: %w", service.Name, err)
	}

	if len(candidates) == 0 {
		return 0, nil, false, fmt.Errorf("service %q: no online cluster members satisfy placement constraints", service.Name)
	}

	if mode == "global" && scaleOverride == nil {
		desired = len(candidates)
	}

	if desired == 0 {
		return 0, nil, false, nil
	}

	if maxReplicas > 0 && uint64(desired) > uint64(len(candidates))*maxReplicas {
		return 0, nil, false, fmt.Errorf(
			"service %q: desired replicas (%d) exceed max_replicas_per_node (%d) across %d matching cluster member(s)",
			service.Name, desired, maxReplicas, len(candidates),
		)
	}

	scales := desired > 1 || mode == "global"
	allowLocal := projectAllowLocal || serviceAllowLocalStorage(service)
	warnLocal := false

	if scales && !env.remotePool {
		if !allowLocal {
			return 0, nil, false, fmt.Errorf(
				"service %q: cannot deploy multiple replicas on a cluster without shared (remote) storage; use a remote storage pool or set x-incus-compose.allow-local-storage: true (--allow-local-storage)",
				service.Name,
			)
		}

		warnLocal = true
	}

	hasPlacement := mode == "global" || len(constraints) > 0 || len(preferences) > 0 || maxReplicas > 0

	// Single replica with no explicit placement co-locates on the connected
	// cluster member when using a bridge network or a local storage pool (#213).
	if desired == 1 && !hasPlacement {
		if env.networkType == "bridge" || !env.remotePool {
			return 1, []string{env.serverName}, warnLocal, nil
		}

		return 1, []string{""}, warnLocal, nil
	}

	if mode == "global" && scaleOverride == nil {
		targets := make([]string, len(candidates))
		for i, m := range candidates {
			targets[i] = m.ServerName
		}

		return desired, targets, warnLocal, nil
	}

	ordered := orderClusterMembers(candidates, preferences, env.serverName)
	targets := make([]string, desired)
	for i := range desired {
		targets[i] = ordered[i%len(ordered)].ServerName
	}

	return desired, targets, warnLocal, nil
}

// isServiceStorageRemote reports whether all storage pools used by the service
// are remote (shared) cluster storage pools.
func (p *Project) isServiceStorageRemote(c *client.Client, service types.ServiceConfig) (bool, error) {
	defaultRemote, err := c.Global().IsStoragePoolRemote(c.Config().DefaultStoragePool)
	if err != nil {
		return false, err
	}
	if !defaultRemote {
		return false, nil
	}

	for _, cVol := range service.Volumes {
		volType := cVol.Type
		if volType == "" && cVol.Source != "" && !strings.HasPrefix(cVol.Source, "/") && !strings.HasPrefix(cVol.Source, ".") {
			volType = "volume"
		}
		if volType != "volume" {
			continue
		}

		volDef := p.Volumes[cVol.Source]
		pool := volumeXIncusComposePool(volDef)
		if pool == "" {
			continue
		}

		remote, err := c.Global().IsStoragePoolRemote(pool)
		if err != nil {
			return false, err
		}
		if !remote {
			return false, nil
		}
	}

	return true, nil
}

// filterClusterMembers filters online cluster members against Compose placement constraints.
func filterClusterMembers(members []incusApi.ClusterMember, constraints []string) ([]incusApi.ClusterMember, error) {
	parsed := make([]placementConstraint, 0, len(constraints))
	for _, raw := range constraints {
		pc, err := parsePlacementConstraint(raw)
		if err != nil {
			return nil, err
		}
		parsed = append(parsed, pc)
	}

	var out []incusApi.ClusterMember
	for _, m := range members {
		if m.Status != "" && !strings.EqualFold(m.Status, "Online") {
			continue
		}

		matched := true
		for _, pc := range parsed {
			if !pc.matches(m) {
				matched = false
				break
			}
		}

		if matched {
			out = append(out, m)
		}
	}

	return out, nil
}

type placementConstraint struct {
	key   string
	op    string
	value string
}

func parsePlacementConstraint(raw string) (placementConstraint, error) {
	for _, op := range []string{"==", "!="} {
		lhs, rhs, ok := strings.Cut(raw, op)
		if !ok {
			continue
		}

		lhs = strings.TrimSpace(lhs)
		rhs = strings.TrimSpace(rhs)
		if lhs == "" {
			break
		}

		switch {
		case lhs == "node.hostname",
			lhs == "node.id",
			lhs == "node.role",
			lhs == "node.group",
			lhs == "node.failure_domain",
			strings.HasPrefix(lhs, "node.labels."):
			return placementConstraint{key: lhs, op: op, value: rhs}, nil
		default:
			return placementConstraint{}, fmt.Errorf("unsupported placement constraint key %q in %q", lhs, raw)
		}
	}

	return placementConstraint{}, fmt.Errorf("invalid placement constraint %q (expected '<key> == <value>' or '<key> != <value>')", raw)
}

func (c placementConstraint) matches(m incusApi.ClusterMember) bool {
	var match bool

	switch {
	case c.key == "node.hostname" || c.key == "node.id":
		match = m.ServerName == c.value
	case c.key == "node.role":
		match = slices.Contains(m.Roles, c.value)
	case c.key == "node.group":
		match = slices.Contains(m.Groups, c.value)
	case c.key == "node.failure_domain":
		match = m.FailureDomain == c.value
	case strings.HasPrefix(c.key, "node.labels."):
		labelKey := strings.TrimPrefix(c.key, "node.labels.")
		match = m.Config["user."+labelKey] == c.value
	}

	if c.op == "!=" {
		return !match
	}

	return match
}

// orderClusterMembers orders candidate cluster members to spread replicas evenly
// across the buckets named by preferences (or by server name, starting with
// the connected server when no spread preference is given).
func orderClusterMembers(candidates []incusApi.ClusterMember, preferences []types.PlacementPreferences, preferredServer string) []incusApi.ClusterMember {
	sorted := slices.Clone(candidates)
	slices.SortFunc(sorted, func(a, b incusApi.ClusterMember) int {
		if a.ServerName == preferredServer && b.ServerName != preferredServer {
			return -1
		}
		if b.ServerName == preferredServer && a.ServerName != preferredServer {
			return 1
		}
		return strings.Compare(a.ServerName, b.ServerName)
	})

	if len(preferences) == 0 || len(sorted) <= 1 {
		return sorted
	}

	spreadKey := strings.TrimSpace(preferences[0].Spread)
	rest := preferences[1:]
	if spreadKey == "" {
		return orderClusterMembers(sorted, rest, preferredServer)
	}

	var bucketOrder []string
	buckets := map[string][]incusApi.ClusterMember{}
	for _, m := range sorted {
		val := memberSpreadValue(m, spreadKey)
		_, exists := buckets[val]
		if !exists {
			bucketOrder = append(bucketOrder, val)
		}
		buckets[val] = append(buckets[val], m)
	}

	if len(rest) > 0 {
		for _, bKey := range bucketOrder {
			buckets[bKey] = orderClusterMembers(buckets[bKey], rest, preferredServer)
		}
	}

	var out []incusApi.ClusterMember
	for len(out) < len(sorted) {
		for _, bKey := range bucketOrder {
			if len(buckets[bKey]) > 0 {
				out = append(out, buckets[bKey][0])
				buckets[bKey] = buckets[bKey][1:]
			}
		}
	}

	return out
}

func memberSpreadValue(m incusApi.ClusterMember, spreadKey string) string {
	switch {
	case spreadKey == "node.failure_domain":
		return m.FailureDomain
	case spreadKey == "node.hostname" || spreadKey == "node.id":
		return m.ServerName
	case spreadKey == "node.group":
		if len(m.Groups) > 0 {
			return m.Groups[0]
		}
		return ""
	case strings.HasPrefix(spreadKey, "node.labels."):
		labelKey := strings.TrimPrefix(spreadKey, "node.labels.")
		return m.Config["user."+labelKey]
	default:
		return m.ServerName
	}
}
