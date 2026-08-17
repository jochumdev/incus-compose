package main

import (
	"slices"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/project"
)

type filterResourcesArgs struct {
	OnlyServices     []string
	WithDependencies bool
	// Reverse includes services that depend on OnlyServices (reverse deps).
	// Use for stop/down; leave false for start/up which only need forward deps.
	Reverse bool

	// IncludeKinds and ExcludeKinds are mutualy exclusive, use one of both.
	IncludeKinds []client.Kind
	ExcludeKinds []client.Kind
}

// discoveredResources returns after minus before, minus the excluded kinds.
func discoveredResources(before []client.Resource, after []client.Resource, exclude []client.Kind) []client.Resource {
	held := make(map[client.Resource]struct{}, len(before))
	for _, r := range before {
		held[r] = struct{}{}
	}

	found := []client.Resource{}

	for _, r := range after {
		_, ok := held[r]
		if ok || slices.Contains(exclude, r.Kind()) {
			continue
		}

		found = append(found, r)
	}

	return found
}

func filterResources(p *project.Project, in map[string][]client.Resource, args filterResourcesArgs) map[string][]client.Resource {
	result := map[string][]client.Resource{}

	if len(args.IncludeKinds) > 0 && len(args.ExcludeKinds) > 0 {
		return nil
	}

	if len(args.OnlyServices) > 0 {
		for _, s := range args.OnlyServices {
			resources, ok := in[s]
			if !ok {
				continue
			}

			result[s] = resources
		}
	} else {
		result = in
	}

	if args.WithDependencies && len(args.OnlyServices) > 0 {
		if args.Reverse {
			// Reverse: pull in services that depend on OnlyServices (for stop/down).
			for _, svc := range p.Services {
				for depName := range svc.DependsOn {
					if !slices.Contains(args.OnlyServices, depName) {
						continue
					}

					resources, ok := in[svc.Name]
					if !ok {
						continue
					}

					result[svc.Name] = resources
				}
			}
		} else {
			// Forward: pull in services that OnlyServices depend on (for start/up).
			for _, s := range args.OnlyServices {
				svc, ok := p.Services[s]
				if !ok {
					continue
				}

				for depName := range svc.DependsOn {
					resources, ok := in[depName]
					if !ok {
						continue
					}

					result[depName] = resources
				}
			}
		}
	}

	if args.ExcludeKinds != nil {
		for n, res := range result {
			newRes := []client.Resource{}

			for _, r := range res {
				if r.Kind() == client.KindInstance || !slices.Contains(args.ExcludeKinds, r.Kind()) {
					newRes = append(newRes, r)
				}
			}

			result[n] = newRes
		}
	} else if args.IncludeKinds != nil {
		for n, res := range result {
			newRes := []client.Resource{}

			for _, r := range res {
				if slices.Contains(args.IncludeKinds, r.Kind()) {
					newRes = append(newRes, r)
				}
			}
			result[n] = newRes
		}
	}

	return result
}
