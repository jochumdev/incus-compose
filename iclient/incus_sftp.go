package iclient

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/pkg/sftp"
)

// incusSFTPMaxPacket is the packet size upstream negotiates; the default is far smaller.
const incusSFTPMaxPacket = 128 * 1024

// GetInstanceFileSFTP returns an SFTP connection to an instance's filesystem.
// The caller closes it; holding one open keeps the forkfile process alive.
func (c *Connection) GetInstanceFileSFTP(ctx context.Context, project string, instanceName string) (*sftp.Client, error) {
	return c.sftpClient(ctx, project, incusInstancePath(instanceName, "/sftp"))
}

// GetStoragePoolVolumeFileSFTP returns an SFTP connection to a custom volume.
// The caller closes it; holding one open keeps the volume mounted, and
// deleting the volume does not wait for that.
func (c *Connection) GetStoragePoolVolumeFileSFTP(ctx context.Context, project string, pool string, volType string, volName string) (*sftp.Client, error) {
	path := "/storage-pools/" + url.PathEscape(pool) +
		"/volumes/" + url.PathEscape(volType) +
		"/" + url.PathEscape(volName) + "/sftp"

	return c.sftpClient(ctx, project, path)
}

// sftpClient upgrades a connection to SFTP and speaks the protocol over it.
func (c *Connection) sftpClient(ctx context.Context, project string, path string) (*sftp.Client, error) {
	conn, err := c.upgradeConn(ctx, project, path, "sftp")
	if err != nil {
		return nil, err
	}

	client, err := sftp.NewClientPipe(conn, conn, sftp.MaxPacketUnchecked(incusSFTPMaxPacket))
	if err != nil {
		_ = conn.Close()

		return nil, fmt.Errorf("starting SFTP on %s: %w", path, err)
	}

	go func() {
		// The pipe does not own the connection, so close it after the client.
		_ = client.Wait()
		_ = conn.Close()
	}()

	return client, nil
}

// upgradeConn dials the server and trades an HTTP request for a raw stream.
// This is not a websocket, and cannot go through the http.Client and its pool.
func (c *Connection) upgradeConn(ctx context.Context, project string, path string, protocol string) (net.Conn, error) {
	transport, ok := c.http.Transport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("unexpected transport %T", c.http.Transport)
	}

	query := url.Values{}
	if project != "" {
		query.Set("project", project)
	}

	uri, err := url.Parse(c.baseURL + "/1.0" + path)
	if err != nil {
		return nil, fmt.Errorf("building the %s address: %w", protocol, err)
	}

	uri.RawQuery = query.Encode()

	conn, err := c.dialRaw(ctx, transport, uri)
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", uri.Host, err)
	}

	req := &http.Request{
		Method:     http.MethodGet,
		URL:        uri,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{},
		Host:       uri.Host,
	}

	req.Header["Upgrade"] = []string{protocol}
	req.Header["Connection"] = []string{"Upgrade"}

	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}

	err = req.Write(conn)
	if err != nil {
		_ = conn.Close()

		return nil, err
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		_ = conn.Close()

		return nil, err
	}

	// On 101 the body is the stream itself, so only a refused upgrade closes it.
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer func() { _ = resp.Body.Close() }()
		defer func() { _ = conn.Close() }()

		return nil, upgradeError(resp)
	}

	if resp.Header.Get("Upgrade") != protocol {
		_ = conn.Close()

		return nil, fmt.Errorf("the server did not upgrade to %s", protocol)
	}

	return conn, nil
}

// dialRaw opens the underlying connection, doing the TLS handshake itself,
// net/http never seeing this one.
func (c *Connection) dialRaw(ctx context.Context, transport *http.Transport, uri *url.URL) (net.Conn, error) {
	addr := uri.Host
	if uri.Port() == "" && uri.Scheme == "https" {
		addr = net.JoinHostPort(uri.Hostname(), "443")
	}

	conn, err := transport.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}

	if transport.TLSClientConfig == nil {
		return conn, nil
	}

	config := transport.TLSClientConfig.Clone()
	if config.ServerName == "" {
		config.ServerName = uri.Hostname()
	}

	tlsConn := tls.Client(conn, config)

	err = tlsConn.HandshakeContext(ctx)
	if err != nil {
		_ = conn.Close()

		return nil, err
	}

	return tlsConn, nil
}

// upgradeError turns a refused upgrade into the server's own error, mapped the
// way every other call maps one.
func upgradeError(resp *http.Response) error {
	decoded := api.Response{}

	err := json.NewDecoder(resp.Body).Decode(&decoded)
	if err != nil || decoded.Error == "" {
		return fmt.Errorf("the server answered %s", resp.Status)
	}

	return api.StatusErrorf(decoded.Code, "%s", decoded.Error)
}
