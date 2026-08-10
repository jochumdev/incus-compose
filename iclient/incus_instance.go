package iclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/lxc/incus/v7/shared/api"
)

// incusInstancesPath is the collection every instance call hangs off.
const incusInstancesPath = "/instances"

// incusInstancesQuery turns the listing arguments into query parameters.
func incusInstancesQuery(args GetInstancesArgs, recursion string) url.Values {
	v := url.Values{}

	if args.Type != api.InstanceTypeAny {
		v.Set("instance-type", string(args.Type))
	}

	if recursion != "" {
		v.Set("recursion", recursion)
	}

	if args.AllProjects {
		v.Set("all-projects", "true")
	}

	filter := incusFilter(args.Filters)
	if filter != "" {
		v.Set("filter", filter)
	}

	return v
}

// incusFilter renders "key=value" selectors the way the API reads them.
func incusFilter(filters []string) string {
	clauses := make([]string, 0, len(filters))

	for _, filter := range filters {
		key, value, found := strings.Cut(filter, "=")
		if !found {
			continue
		}

		clauses = append(clauses, key+" eq "+value)
	}

	return strings.Join(clauses, " and ")
}

// incusRecursion is the depth a Full listing needs.
func incusRecursion(full bool) string {
	if full {
		return "2"
	}

	return "1"
}

// incusInstancePath escapes a name into a path under the collection.
func incusInstancePath(name string, suffix ...string) string {
	return incusInstancesPath + "/" + url.PathEscape(name) + strings.Join(suffix, "")
}

// GetInstance returns one instance and its ETag. Without args.Full the state,
// snapshots and backups are zero.
func (c *Connection) GetInstance(ctx context.Context, name string, args *GetInstanceArgs) (*api.InstanceFull, string, error) {
	if args == nil {
		args = &GetInstanceArgs{}
	}

	instance := api.InstanceFull{}

	var query url.Values

	if args.Full {
		query = url.Values{}
		query.Set("recursion", "1")
	}

	etag, err := c.getStruct(ctx, incusInstancePath(name), query, &instance)
	if err != nil {
		return nil, "", err
	}

	return &instance, etag, nil
}

// GetInstances returns the instances the arguments select.
func (c *Connection) GetInstances(ctx context.Context, args *GetInstancesArgs) ([]api.InstanceFull, error) {
	if args == nil {
		args = &GetInstancesArgs{}
	}

	instances := []api.InstanceFull{}

	_, err := c.getStruct(ctx, incusInstancesPath, incusInstancesQuery(*args, incusRecursion(args.Full)), &instances)
	if err != nil {
		return nil, err
	}

	return instances, nil
}

// GetInstanceNames returns the names of the instances the arguments select.
// AllProjects is refused, a bare name not being unique across projects.
func (c *Connection) GetInstanceNames(ctx context.Context, args *GetInstancesArgs) ([]string, error) {
	if args == nil {
		args = &GetInstancesArgs{}
	}

	if args.AllProjects {
		return nil, fmt.Errorf("listing instance names across projects: %w", ErrConnectionUnsupported)
	}

	// Without recursion the collection is a list of resource URLs.
	uris := []string{}

	_, err := c.getStruct(ctx, incusInstancesPath, incusInstancesQuery(*args, ""), &uris)
	if err != nil {
		return nil, err
	}

	return resourceNames(incusInstancesPath, uris)
}

// CreateInstance creates an instance and follows the operation.
func (c *Connection) CreateInstance(ctx context.Context, instance api.InstancesPost) (<-chan api.Operation, error) {
	return c.asyncOperation(ctx, http.MethodPost, incusInstancesPath, instance, "")
}

// DeleteInstance removes an instance and follows the operation.
func (c *Connection) DeleteInstance(ctx context.Context, name string) (<-chan api.Operation, error) {
	return c.asyncOperation(ctx, http.MethodDelete, incusInstancePath(name), nil, "")
}

// UpdateInstanceState starts, stops or restarts an instance and follows the
// operation.
func (c *Connection) UpdateInstanceState(ctx context.Context, name string, state api.InstanceStatePut, etag string) (<-chan api.Operation, error) {
	return c.asyncOperation(ctx, http.MethodPut, incusInstancePath(name, "/state"), state, etag)
}

// GetInstanceState returns the runtime state of one instance.
func (c *Connection) GetInstanceState(ctx context.Context, name string) (*api.InstanceState, string, error) {
	state := api.InstanceState{}

	etag, err := c.getStruct(ctx, incusInstancePath(name, "/state"), nil, &state)
	if err != nil {
		return nil, "", err
	}

	return &state, etag, nil
}

// WaitInstanceBusy blocks until no queryable operation holds the instance's
// operation lock.
//
// Incus takes that lock in the driver, inside the operation, so a write issued
// while it is held is accepted and then fails from the operation.
func (c *Connection) WaitInstanceBusy(ctx context.Context, name string) error {
	// Outside the default project the operation's resource URL carries
	// ?project=, and matching a bare path reports every instance as free.
	instanceURL := api.NewURL().Path("1.0", "instances", name).Project(c.project).String()

	for {
		err := ctx.Err()
		if err != nil {
			return err
		}

		operations, err := c.GetOperations(ctx)
		if err != nil {
			return fmt.Errorf("listing the operations on %q: %w", name, err)
		}

		holder := api.Operation{}

		for _, operation := range operations {
			if operation.Class != api.OperationClassTask || operation.StatusCode.IsFinal() {
				continue
			}

			if slices.Contains(operation.Resources["instances"], instanceURL) {
				holder = operation

				break
			}
		}

		if holder.ID == "" {
			return nil
		}

		// How it ended is the business of whoever started it; this only waits.
		_, err = c.WaitOperationID(ctx, holder.ID)
		if err != nil {
			// Gone between the listing and here means it has finished.
			if api.StatusErrorCheck(err, http.StatusNotFound) {
				continue
			}

			return fmt.Errorf("waiting for operation %s on %q: %w", holder.ID, name, err)
		}
	}
}

// PatchInstanceConfig merges keys into an instance's config, leaving the rest
// of the instance alone. No cluster target is sent; instance config is
// cluster-wide state.
func (c *Connection) PatchInstanceConfig(ctx context.Context, name string, config map[string]string) error {
	patch := struct {
		Config map[string]string `json:"config"`
	}{Config: config}

	_, _, err := c.do(ctx, http.MethodPatch, incusInstancePath(name), nil, patch, "")

	return err
}
