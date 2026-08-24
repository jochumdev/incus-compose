package iclient

import (
	"context"
	"net/http"
	"net/url"

	"github.com/lxc/incus/v7/shared/api"
)

// incusCertificatesPath is the collection every certificate call hangs off.
const incusCertificatesPath = "/certificates"

// GetCertificates returns the server's trust store.
func (c *Connection) GetCertificates(ctx context.Context) ([]api.Certificate, error) {
	certificates := []api.Certificate{}

	query := url.Values{}
	query.Set("recursion", "1")

	_, err := c.getStruct(ctx, "", incusCertificatesPath, query, &certificates)
	if err != nil {
		return nil, err
	}

	return certificates, nil
}

// CreateCertificate adds a certificate to the trust store.
//
// With certificate.TrustToken set this is the registration call, the one
// request a client makes before it is trusted.
func (c *Connection) CreateCertificate(ctx context.Context, certificate api.CertificatesPost) error {
	_, _, err := c.do(ctx, "", http.MethodPost, incusCertificatesPath, nil, certificate, "")

	return err
}

// CreateCertificateToken mints a trust token for someone else to register with.
//
// This is a token operation: it never reaches a terminal state. Read the first
// value, whose Metadata carries the secret, and cancel the context.
func (c *Connection) CreateCertificateToken(ctx context.Context, certificate api.CertificatesPost) (<-chan api.Operation, error) {
	certificate.Token = true

	return c.asyncOperation(ctx, "", http.MethodPost, incusCertificatesPath, certificate, "")
}

// DeleteCertificate removes a certificate from the trust store.
func (c *Connection) DeleteCertificate(ctx context.Context, fingerprint string) error {
	_, _, err := c.do(ctx, "", http.MethodDelete, incusCertificatesPath+"/"+url.PathEscape(fingerprint), nil, nil, "")

	return err
}
