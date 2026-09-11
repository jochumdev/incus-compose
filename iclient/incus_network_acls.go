package iclient

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/lxc/incus/v7/shared/api"
)

// incusNetworkACLsPath is the collection every network ACL call hangs off.
const incusNetworkACLsPath = "/network-acls"

// isTransientACLRace reports whether err is an upstream Incus race where
// acl.UsedBy fails because a concurrently deleted project was not found.
// Remove when https://github.com/lxc/incus/issues/3983 is merged and live in the LTS release.
func isTransientACLRace(err error) bool {
	if err == nil {
		return false
	}

	msg := err.Error()

	return strings.Contains(msg, "Failed getting ACL usage") ||
		strings.Contains(msg, "Failed to load project")
}

// retryACLOp runs an ACL operation with retries when hitting the upstream Incus
// race where UsedBy scans an unrelated project being deleted concurrently.
// Remove when https://github.com/lxc/incus/issues/3983 is merged and live in the LTS release.
func retryACLOp(ctx context.Context, op func() error) error {
	return retry.New(
		retry.Context(ctx),
		retry.Attempts(5),
		retry.Delay(100*time.Millisecond),
		retry.DelayType(retry.BackOffDelay),
		retry.LastErrorOnly(true),
		retry.RetryIf(isTransientACLRace),
	).Do(op)
}

// GetNetworkACLNames returns the names of every network ACL.
func (c *Connection) GetNetworkACLNames(ctx context.Context, project string) ([]string, error) {
	uris := []string{}

	_, err := c.getStruct(ctx, project, incusNetworkACLsPath, nil, &uris)
	if err != nil {
		return nil, err
	}

	return resourceNames(incusNetworkACLsPath, uris)
}

// GetNetworkACLs returns every network ACL, each one whole.
func (c *Connection) GetNetworkACLs(ctx context.Context, project string) ([]api.NetworkACL, error) {
	acls := []api.NetworkACL{}

	query := url.Values{}
	query.Set("recursion", "1")

	err := retryACLOp(ctx, func() error {
		_, opErr := c.getStruct(ctx, project, incusNetworkACLsPath, query, &acls)

		return opErr
	})
	if err != nil {
		return nil, err
	}

	return acls, nil
}

// GetNetworkACL returns one network ACL and its ETag.
func (c *Connection) GetNetworkACL(ctx context.Context, project string, name string) (*api.NetworkACL, string, error) {
	acl := api.NetworkACL{}
	var etag string

	err := retryACLOp(ctx, func() error {
		var opErr error
		etag, opErr = c.getStruct(ctx, project, incusNetworkACLsPath+"/"+url.PathEscape(name), nil, &acl)

		return opErr
	})
	if err != nil {
		return nil, "", err
	}

	return &acl, etag, nil
}

// CreateNetworkACL adds a network ACL.
func (c *Connection) CreateNetworkACL(ctx context.Context, project string, acl api.NetworkACLsPost) error {
	return retryACLOp(ctx, func() error {
		_, _, err := c.do(ctx, project, http.MethodPost, incusNetworkACLsPath, nil, acl, "")

		return err
	})
}

// UpdateNetworkACL replaces a network ACL's rules.
func (c *Connection) UpdateNetworkACL(ctx context.Context, project string, name string, acl api.NetworkACLPut, etag string) error {
	return retryACLOp(ctx, func() error {
		_, _, err := c.do(ctx, project, http.MethodPut, incusNetworkACLsPath+"/"+url.PathEscape(name), nil, acl, etag)

		return err
	})
}

// DeleteNetworkACL removes a network ACL.
func (c *Connection) DeleteNetworkACL(ctx context.Context, project string, name string) error {
	return retryACLOp(ctx, func() error {
		_, _, err := c.do(ctx, project, http.MethodDelete, incusNetworkACLsPath+"/"+url.PathEscape(name), nil, nil, "")

		return err
	})
}
