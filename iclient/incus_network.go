package iclient

import (
	"context"
	"net/http"
	"net/url"

	"github.com/lxc/incus/v7/shared/api"
)

// incusNetworksPath is the collection every network call hangs off.
const incusNetworksPath = "/networks"

// GetNetwork returns one network and its ETag.
func (c *Connection) GetNetwork(ctx context.Context, project string, name string) (*api.Network, string, error) {
	network := api.Network{}

	etag, err := c.getStruct(ctx, project, incusNetworksPath+"/"+url.PathEscape(name), nil, &network)
	if err != nil {
		return nil, "", err
	}

	return &network, etag, nil
}

// GetNetworkNames returns the names of every network.
func (c *Connection) GetNetworkNames(ctx context.Context, project string) ([]string, error) {
	uris := []string{}

	_, err := c.getStruct(ctx, project, incusNetworksPath, nil, &uris)
	if err != nil {
		return nil, err
	}

	return resourceNames(incusNetworksPath, uris)
}

// GetNetworks returns every network, each one whole.
func (c *Connection) GetNetworks(ctx context.Context, project string) ([]api.Network, error) {
	networks := []api.Network{}

	query := url.Values{}
	query.Set("recursion", "1")

	_, err := c.getStruct(ctx, project, incusNetworksPath, query, &networks)
	if err != nil {
		return nil, err
	}

	return networks, nil
}

// CreateNetwork adds a managed network.
func (c *Connection) CreateNetwork(ctx context.Context, project string, network api.NetworksPost) error {
	_, _, err := c.do(ctx, project, http.MethodPost, incusNetworksPath, nil, network, "")

	return err
}

// UpdateNetwork replaces a network's configuration.
func (c *Connection) UpdateNetwork(ctx context.Context, project string, name string, network api.NetworkPut, etag string) error {
	_, _, err := c.do(ctx, project, http.MethodPut, incusNetworksPath+"/"+url.PathEscape(name), nil, network, etag)

	return err
}

// DeleteNetwork removes a managed network.
func (c *Connection) DeleteNetwork(ctx context.Context, project string, name string) error {
	_, _, err := c.do(ctx, project, http.MethodDelete, incusNetworksPath+"/"+url.PathEscape(name), nil, nil, "")

	return err
}
