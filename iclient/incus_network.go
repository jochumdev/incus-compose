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
func (c *Connection) GetNetwork(ctx context.Context, name string) (*api.Network, string, error) {
	network := api.Network{}

	etag, err := c.getStruct(ctx, incusNetworksPath+"/"+url.PathEscape(name), nil, &network)
	if err != nil {
		return nil, "", err
	}

	return &network, etag, nil
}

// GetNetworkNames returns the names of every network.
func (c *Connection) GetNetworkNames(ctx context.Context) ([]string, error) {
	uris := []string{}

	_, err := c.getStruct(ctx, incusNetworksPath, nil, &uris)
	if err != nil {
		return nil, err
	}

	return resourceNames(incusNetworksPath, uris)
}

// GetNetworks returns every network, each one whole.
func (c *Connection) GetNetworks(ctx context.Context) ([]api.Network, error) {
	networks := []api.Network{}

	query := url.Values{}
	query.Set("recursion", "1")

	_, err := c.getStruct(ctx, incusNetworksPath, query, &networks)
	if err != nil {
		return nil, err
	}

	return networks, nil
}

// CreateNetwork adds a managed network.
func (c *Connection) CreateNetwork(ctx context.Context, network api.NetworksPost) error {
	_, _, err := c.do(ctx, http.MethodPost, incusNetworksPath, nil, network, "")

	return err
}

// UpdateNetwork replaces a network's configuration.
func (c *Connection) UpdateNetwork(ctx context.Context, name string, network api.NetworkPut, etag string) error {
	_, _, err := c.do(ctx, http.MethodPut, incusNetworksPath+"/"+url.PathEscape(name), nil, network, etag)

	return err
}

// DeleteNetwork removes a managed network.
func (c *Connection) DeleteNetwork(ctx context.Context, name string) error {
	_, _, err := c.do(ctx, http.MethodDelete, incusNetworksPath+"/"+url.PathEscape(name), nil, nil, "")

	return err
}
