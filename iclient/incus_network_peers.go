package iclient

import (
	"context"
	"net/http"
	"net/url"

	"github.com/lxc/incus/v7/shared/api"
)

// incusNetworkPeersPath is the collection a network's peer calls hang off.
func incusNetworkPeersPath(network string) string {
	return incusNetworksPath + "/" + url.PathEscape(network) + "/peers"
}

// GetNetworkPeer returns one network peer and its ETag.
func (c *Connection) GetNetworkPeer(ctx context.Context, project string, network string, name string) (*api.NetworkPeer, string, error) {
	peer := api.NetworkPeer{}

	etag, err := c.getStruct(ctx, project, incusNetworkPeersPath(network)+"/"+url.PathEscape(name), nil, &peer)
	if err != nil {
		return nil, "", err
	}

	return &peer, etag, nil
}

// GetNetworkPeers returns every peer of a network.
func (c *Connection) GetNetworkPeers(ctx context.Context, project string, network string) ([]api.NetworkPeer, error) {
	peers := []api.NetworkPeer{}

	query := url.Values{}
	query.Set("recursion", "1")

	_, err := c.getStruct(ctx, project, incusNetworkPeersPath(network), query, &peers)
	if err != nil {
		return nil, err
	}

	return peers, nil
}

// CreateNetworkPeer adds a network peer, initiating the relationship.
func (c *Connection) CreateNetworkPeer(ctx context.Context, project string, network string, peer api.NetworkPeersPost) error {
	// Remove when https://github.com/lxc/incus/issues/3983 is merged and live in the LTS release.
	return retryACLOp(ctx, func() error {
		_, _, err := c.do(ctx, project, http.MethodPost, incusNetworkPeersPath(network), nil, peer, "")

		return err
	})
}

// UpdateNetworkPeer replaces a network peer's configuration.
func (c *Connection) UpdateNetworkPeer(ctx context.Context, project string, network string, name string, peer api.NetworkPeerPut, etag string) error {
	// Remove when https://github.com/lxc/incus/issues/3983 is merged and live in the LTS release.
	return retryACLOp(ctx, func() error {
		_, _, err := c.do(ctx, project, http.MethodPut, incusNetworkPeersPath(network)+"/"+url.PathEscape(name), nil, peer, etag)

		return err
	})
}

// DeleteNetworkPeer removes a network peer.
func (c *Connection) DeleteNetworkPeer(ctx context.Context, project string, network string, name string) error {
	// Remove when https://github.com/lxc/incus/issues/3983 is merged and live in the LTS release.
	return retryACLOp(ctx, func() error {
		_, _, err := c.do(ctx, project, http.MethodDelete, incusNetworkPeersPath(network)+"/"+url.PathEscape(name), nil, nil, "")

		return err
	})
}
