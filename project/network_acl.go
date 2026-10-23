package project

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	incusApi "github.com/lxc/incus/v7/shared/api"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/shared"
)

type xicNetworkConfig struct {
	Parent string         `mapstructure:"parent"`
	Uplink string         `mapstructure:"uplink"`
	ACL    *xicNetworkACL `mapstructure:"acl"`
}

type xicNetworkACL struct {
	Default xicNetworkACLDefault `mapstructure:"default"`
	Ingress []xicNetworkACLRule  `mapstructure:"ingress"`
	Egress  []xicNetworkACLRule  `mapstructure:"egress"`
}

type xicNetworkACLDefault struct {
	Ingress string `mapstructure:"ingress"`
	Egress  string `mapstructure:"egress"`
}

type xicNetworkACLRule struct {
	From        string   `mapstructure:"from"`
	To          string   `mapstructure:"to"`
	Ports       []string `mapstructure:"ports"`
	Action      string   `mapstructure:"action"`
	State       string   `mapstructure:"state"`
	Description string   `mapstructure:"description"`
}

type portGroup struct {
	protocol string
	ports    string
}

var (
	validACLDefaultActions = []string{"allow", "reject", "drop"}
	validACLRuleActions    = []string{"allow", "reject", "drop", "allow-stateless"}
	validACLRuleStates     = []string{"enabled", "logged", "disabled"}
)

func configureNetworkACL(c *client.Client, p *types.Project, name, serviceName, uplink string, networkDef types.NetworkConfig, network *client.Network) error {
	if networkDef.Extensions == nil {
		return nil
	}

	var xic xicNetworkConfig
	ok, err := networkDef.Extensions.Get("x-incus-compose", &xic)
	if err != nil {
		return fmt.Errorf("parsing x-incus-compose: %w", err)
	}

	if !ok || xic.ACL == nil {
		return nil
	}

	if !c.Global().HasExtension(shared.Incus75Extension) {
		c.LogWarn(
			"For a network ACL you need at least incus 7.5 or 7.0.2 LTS",
			"service", serviceName,
			"network", name,
		)

		return nil
	}

	aclCfg := xic.ACL
	isOVN := network.Config.Type == "ovn"

	if !isOVN && (aclCfg.Default.Ingress != "" || aclCfg.Default.Egress != "") {
		return fmt.Errorf("acl.default is only supported on OVN networks")
	}

	if aclCfg.Default.Ingress != "" && !slices.Contains(validACLDefaultActions, aclCfg.Default.Ingress) {
		return fmt.Errorf("invalid acl.default.ingress %q: must be allow, reject, or drop", aclCfg.Default.Ingress)
	}

	if aclCfg.Default.Egress != "" && !slices.Contains(validACLDefaultActions, aclCfg.Default.Egress) {
		return fmt.Errorf("invalid acl.default.egress %q: must be allow, reject, or drop", aclCfg.Default.Egress)
	}

	if network.Config.Extensions == nil {
		network.Config.Extensions = map[string]string{}
	}

	if aclCfg.Default.Ingress != "" {
		network.Config.Extensions["security.acls.default.ingress.action"] = aclCfg.Default.Ingress
	}

	if len(aclCfg.Ingress) == 0 && aclCfg.Default.Ingress == "allow" {
		c.LogWarn(
			"Network ACL default ingress action is \"allow\" without ingress rules; inbound traffic is unrestricted",
			"network", name,
		)
	}

	if aclCfg.Default.Egress != "" {
		network.Config.Extensions["security.acls.default.egress.action"] = aclCfg.Default.Egress
	}

	if len(aclCfg.Egress) == 0 && aclCfg.Default.Egress != "reject" && aclCfg.Default.Egress != "drop" {
		c.LogWarn(
			"Network ACL defines no egress restrictions; outbound traffic remains allowed by default",
			"network", name,
		)
	}

	network.Config.ACL = &incusApi.NetworkACLsPost{
		NetworkACLPost: incusApi.NetworkACLPost{
			Name: network.IncusName(),
		},
		NetworkACLPut: incusApi.NetworkACLPut{
			Description: fmt.Sprintf(c.Config().DescriptionFormat, name),
		},
	}

	aclNetworks := map[string]*client.Network{}

	ingressRules, err := compileACLRules(c, p, "ingress", serviceName, uplink, isOVN, aclCfg.Ingress, aclNetworks)
	if err != nil {
		network.Config.ACL = nil
		return err
	}

	egressRules, err := compileACLRules(c, p, "egress", serviceName, uplink, isOVN, aclCfg.Egress, aclNetworks)
	if err != nil {
		network.Config.ACL = nil
		return err
	}

	network.Config.ACL.Ingress = ingressRules
	network.Config.ACL.Egress = egressRules

	if len(aclNetworks) > 0 {
		network.Config.ACLNetworks = aclNetworks
	}

	return nil
}

func compileACLRules(c *client.Client, p *types.Project, direction, serviceName, uplink string, isOVN bool, rules []xicNetworkACLRule, aclNetworks map[string]*client.Network) ([]incusApi.NetworkACLRule, error) {
	out := []incusApi.NetworkACLRule{}

	for _, rule := range rules {
		var rawTarget string
		if direction == "ingress" {
			if rule.To != "" {
				return nil, fmt.Errorf("ingress rule cannot specify \"to\", use \"from\"")
			}
			rawTarget = strings.TrimSpace(rule.From)
		} else {
			if rule.From != "" {
				return nil, fmt.Errorf("egress rule cannot specify \"from\", use \"to\"")
			}
			rawTarget = strings.TrimSpace(rule.To)
		}

		action := rule.Action
		if action == "" {
			action = "allow"
		}
		if !slices.Contains(validACLRuleActions, action) {
			return nil, fmt.Errorf("invalid acl rule action %q: must be allow, reject, drop, or allow-stateless", rule.Action)
		}
		if !isOVN && action != "allow" {
			return nil, fmt.Errorf("acl rule action %q is only supported on OVN networks", rule.Action)
		}

		state := rule.State
		if state == "" {
			state = "enabled"
		}
		if !slices.Contains(validACLRuleStates, state) {
			return nil, fmt.Errorf("invalid acl rule state %q: must be enabled, logged, or disabled", rule.State)
		}
		if !isOVN && state != "enabled" {
			return nil, fmt.Errorf("acl rule state %q is only supported on OVN networks", rule.State)
		}

		subject, err := resolveACLTarget(c, p, rawTarget, serviceName, uplink, isOVN, aclNetworks)
		if err != nil {
			return nil, err
		}

		groups, err := parsePortSpecs(rule.Ports)
		if err != nil {
			return nil, err
		}

		targetLabel := rawTarget
		if targetLabel == "" {
			targetLabel = "any"
		}

		prep := "from"
		if direction == "egress" {
			prep = "to"
		}

		for _, pg := range groups {
			desc := rule.Description
			if desc == "" {
				var inner string
				switch {
				case pg.protocol == "":
					inner = fmt.Sprintf("%s %s %s", direction, prep, targetLabel)
				case pg.ports == "":
					inner = fmt.Sprintf("%s %s %s (%s)", direction, prep, targetLabel, pg.protocol)
				default:
					inner = fmt.Sprintf("%s %s %s (%s/%s)", direction, prep, targetLabel, pg.protocol, pg.ports)
				}
				desc = fmt.Sprintf(c.Config().DescriptionFormat, inner)
			}

			aclRule := incusApi.NetworkACLRule{
				Action:          action,
				State:           state,
				Protocol:        pg.protocol,
				DestinationPort: pg.ports,
				Description:     desc,
			}
			if direction == "ingress" {
				aclRule.Source = subject
			} else {
				aclRule.Destination = subject
			}

			out = append(out, aclRule)
		}
	}

	return out, nil
}

func resolveACLTarget(c *client.Client, p *types.Project, target, serviceName, uplink string, isOVN bool, aclNetworks map[string]*client.Network) (string, error) {
	if target == "" || target == "any" {
		return "", nil
	}

	if target == "@internal" || target == "@external" {
		if !isOVN {
			return "", fmt.Errorf("acl selector %q is only supported on OVN networks", target)
		}

		return target, nil
	}

	if p != nil {
		if _, ok := p.Networks[target]; ok {
			net, _, _, _, err := ensureProjectNetwork(c, p, target, serviceName, uplink)
			if err != nil {
				return "", err
			}

			aclNetworks[target] = net

			return target, nil
		}
	}

	if isIPLiteral(target) {
		return target, nil
	}

	if strings.Contains(target, ":") {
		owner, netName, _ := strings.Cut(target, ":")
		if owner == "" || netName == "" || strings.Contains(netName, ":") {
			return "", fmt.Errorf("invalid cross-project network reference %q: must be `<project>:<network>`", target)
		}

		owned, err := c.Global().EnsureProject(owner)
		if err != nil {
			return "", err
		}

		rNet, err := owned.Resource(client.KindNetwork, netName, &client.NetworkConfig{External: true})
		if err != nil {
			return "", err
		}

		targetNet, ok := rNet.(*client.Network)
		if !ok {
			return "", fmt.Errorf("failed to resolve network %q", target)
		}

		aclNetworks[target] = targetNet

		return target, nil
	}

	return "", fmt.Errorf("unknown ACL target %q", target)
}

func parsePortSpecs(specs []string) ([]portGroup, error) {
	if len(specs) == 0 {
		return []portGroup{{}}, nil
	}

	var order []string
	byProto := map[string][]string{}
	wildcard := map[string]bool{}

	for _, rawSpec := range specs {
		spec := strings.TrimSpace(rawSpec)
		if spec == "" {
			return nil, fmt.Errorf("empty port specification")
		}

		if strings.Count(spec, "/") > 1 {
			return nil, fmt.Errorf("invalid port specification %q", rawSpec)
		}

		portPart, proto, hasProto := strings.Cut(spec, "/")
		if !hasProto {
			proto = "tcp"
		} else {
			proto = strings.ToLower(strings.TrimSpace(proto))
			portPart = strings.TrimSpace(portPart)
		}

		if portPart == "" || proto == "" {
			return nil, fmt.Errorf("invalid port specification %q", rawSpec)
		}

		switch proto {
		case "tcp", "udp":
			if portPart == "*" {
				wildcard[proto] = true
			} else {
				err := validatePortOrRange(portPart)
				if err != nil {
					return nil, fmt.Errorf("invalid port specification %q: %w", rawSpec, err)
				}
				byProto[proto] = append(byProto[proto], portPart)
			}
		case "icmp4", "icmp6":
			if portPart != "*" {
				return nil, fmt.Errorf("protocol %q only supports wildcard port \"*\" (e.g. \"*/%s\"), got %q", proto, proto, rawSpec)
			}
			wildcard[proto] = true
		default:
			return nil, fmt.Errorf("unsupported protocol %q in port specification %q", proto, rawSpec)
		}

		if !slices.Contains(order, proto) {
			order = append(order, proto)
		}
	}

	out := make([]portGroup, 0, len(order))
	for _, proto := range order {
		ports := ""
		if !wildcard[proto] {
			ports = strings.Join(byProto[proto], ",")
		}
		out = append(out, portGroup{protocol: proto, ports: ports})
	}

	return out, nil
}

func validatePortOrRange(s string) error {
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return fmt.Errorf("empty port")
		}

		startStr, endStr, isRange := strings.Cut(part, "-")
		if !isRange {
			port, err := strconv.ParseUint(part, 10, 16)
			if err != nil || port == 0 {
				return fmt.Errorf("port %q must be between 1 and 65535", part)
			}

			continue
		}

		start, err1 := strconv.ParseUint(strings.TrimSpace(startStr), 10, 16)
		end, err2 := strconv.ParseUint(strings.TrimSpace(endStr), 10, 16)
		if err1 != nil || err2 != nil || start == 0 || end == 0 || start > end {
			return fmt.Errorf("port range %q must be start-end between 1 and 65535", part)
		}
	}

	return nil
}

func isIPLiteral(s string) bool {
	parts := strings.Split(s, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return false
		}

		_, err := netip.ParsePrefix(part)
		if err == nil {
			continue
		}

		_, err = netip.ParseAddr(part)
		if err == nil {
			continue
		}

		startStr, endStr, ok := strings.Cut(part, "-")
		if !ok {
			return false
		}

		startIP, err1 := netip.ParseAddr(strings.TrimSpace(startStr))
		endIP, err2 := netip.ParseAddr(strings.TrimSpace(endStr))
		if err1 != nil || err2 != nil || startIP.Is4() != endIP.Is4() || startIP.Compare(endIP) > 0 {
			return false
		}
	}

	return len(parts) > 0
}
