package iclient

import (
	"context"
	"slices"
	"strings"

	"github.com/lxc/incus/v7/shared/api"
)

// GetServer returns the server's own description and its ETag.
func (c *Connection) GetServer(ctx context.Context) (*api.Server, string, error) {
	server := api.Server{}

	// The root of the API is /1.0 itself.
	etag, err := c.getStruct(ctx, "", nil, &server)
	if err != nil {
		return nil, "", err
	}

	return &server, etag, nil
}

// HasExtension reports whether the server carries an API extension. It asks
// every time: a memo would be the one piece of mutable state on the connection.
func (c *Connection) HasExtension(ctx context.Context, extension string) (bool, error) {
	server, _, err := c.GetServer(ctx)
	if err != nil {
		return false, err
	}

	return slices.Contains(server.APIExtensions, extension), nil
}

// GetConnectionInfo describes how this connection reaches the server.
func (c *Connection) GetConnectionInfo(ctx context.Context) (*ConnectionInfo, error) {
	info := &ConnectionInfo{
		Certificate: c.serverCert,
		Protocol:    "incus",
		URL:         c.baseURL,
		SocketPath:  c.socketPath,
		Project:     c.project,
	}

	if info.Project == "" {
		info.Project = api.ProjectDefaultName
	}

	server, _, err := c.GetServer(ctx)
	if err != nil {
		return nil, err
	}

	info.Target = server.Environment.ServerName

	if c.socketPath == "" {
		info.Addresses = append(info.Addresses, c.baseURL)
	}

	for _, addr := range server.Environment.Addresses {
		// A wildcard address names no host, so it reaches nothing.
		if strings.HasPrefix(addr, ":") {
			continue
		}

		info.Addresses = append(info.Addresses, "https://"+addr)
	}

	return info, nil
}

// RawQuery sends a request to a path the caller built in full: the path is
// used verbatim, with no /1.0 prefix and no project.
func (c *Connection) RawQuery(ctx context.Context, method string, path string, data any, queryETag string) (*api.Response, string, error) {
	return c.doPath(ctx, method, path, data, queryETag)
}
