// Package incustrust turns configuration into an authenticated Incus
// connection, enrolling in the daemon's trust store on first run.
//
// Three ways in, tried most-explicit first: a certificate and key it was handed,
// a one-time trust token, or a remote out of the Incus CLI's configuration.
package incustrust

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	incusapi "github.com/lxc/incus/v7/shared/api"

	"github.com/lxc/incus-compose/iclient"
)

// File names under the directories Config names.
const (
	// CertFile and KeyFile are the enrolled pair, in DataDir. It has to survive
	// a restart: a token is spent the first time it is redeemed.
	CertFile = "client.crt"
	KeyFile  = "client.key"

	// TokenFile is the one-time trust token, in SecretsDir rather than DataDir
	// because a tmpfs is the right place for it.
	TokenFile = "token"
)

// enrollTimeout bounds redeeming the token, which nothing else watches.
const enrollTimeout = 30 * time.Second

// ErrNoCredentials reports configuration that cannot reach Incus: no
// certificate to present, no token to earn one with, and no remote to read
// either from.
var ErrNoCredentials = errors.New("no certificate, no trust token and no remote")

// Config is what a binary knows about how to reach Incus. The more explicit an
// answer is the earlier it is taken, so a mounted certificate is never quietly
// overridden by a stray remote.
type Config struct {
	// Name identifies this client, in the Incus trust store and in the user
	// agent. Two binaries against one daemon want two names.
	Name string

	// UserAgent is what the daemon logs the connection as. Name when empty.
	UserAgent string

	// URL is the Incus endpoint. Required for everything but Remote.
	URL string

	// ClientCert and ClientKey name a pair to present, both or neither. Already
	// trusted, so there is nothing to redeem and nothing to persist.
	ClientCert string
	ClientKey  string

	// Token is a one-time trust token. TokenFile under SecretsDir is read when
	// this is empty, which is what a compose secret mounts.
	Token string

	// DataDir holds the pair generated on enrollment. It must outlive the
	// container: the token is spent, so a lost certificate cannot be re-earned.
	DataDir string

	// SecretsDir holds the token file.
	SecretsDir string

	// Restricted asks Incus for a certificate confined to Projects. Off by
	// default: a restricted certificate answers NXDOMAIN for everything else,
	// which is indistinguishable from correct isolation.
	Restricted bool

	// Projects is what a restricted certificate is confined to. Ignored unless
	// Restricted.
	Projects []string

	// Remote names an entry in the Incus CLI's configuration to connect as.
	// Empty means the default remote, and it is reached only when there is no
	// certificate and no token.
	Remote string

	// UseRemote allows the CLI configuration to be used at all. Off by default:
	// losing a certificate should fail rather than quietly connect as whatever
	// identity a home directory holds.
	UseRemote bool
}

// agent is the user agent to present.
func (c Config) agent() string {
	if c.UserAgent != "" {
		return c.UserAgent
	}

	return c.Name
}

// Connect returns a connection ready to use, enrolling on first run. Nothing is
// dialed except by enrollment, which is the one call here that proves the
// endpoint is reachable and that we are trusted.
func Connect(ctx context.Context, cfg Config) (*iclient.Connection, error) {
	// An explicit pair is already trusted. Nothing to redeem, nothing to write.
	if cfg.ClientCert != "" || cfg.ClientKey != "" {
		if cfg.ClientCert == "" || cfg.ClientKey == "" {
			return nil, errors.New("client certificate and key are both or neither")
		}

		certPEM, keyPEM, err := readPair(cfg.ClientCert, cfg.ClientKey)
		if err != nil {
			return nil, err
		}

		return dial(cfg, certPEM, keyPEM)
	}

	// A pair enrolled on an earlier run, checked before the token because the
	// token file usually outlives the run that spent it. Only with a directory
	// to look in: an empty DataDir would read a relative path out of the working
	// directory and connect as whatever sits there.
	if cfg.DataDir != "" {
		certPEM, keyPEM, err := readPair(
			filepath.Join(cfg.DataDir, CertFile),
			filepath.Join(cfg.DataDir, KeyFile),
		)
		if err == nil {
			return dial(cfg, certPEM, keyPEM)
		}

		if !os.IsNotExist(err) {
			return nil, err
		}
	}

	token, err := token(cfg)
	if err != nil {
		return nil, err
	}

	if token != "" {
		return Enroll(ctx, cfg, token)
	}

	if cfg.UseRemote {
		return remote(cfg)
	}

	return nil, ErrNoCredentials
}

// Enroll generates a pair, has Incus trust it with the token, and persists it -
// only after the daemon accepted it, since a refused pair would leave a
// certificate on disk with no token left to fix it with.
func Enroll(ctx context.Context, cfg Config, token string) (*iclient.Connection, error) {
	certPEM, keyPEM, err := generate(cfg.Name)
	if err != nil {
		return nil, err
	}

	conn, err := dial(cfg, certPEM, keyPEM)
	if err != nil {
		return nil, err
	}

	post := incusapi.CertificatesPost{
		CertificatePut: incusapi.CertificatePut{Name: cfg.Name},
		TrustToken:     token,
	}

	// A restricted certificate needs the list it is restricted to.
	if cfg.Restricted && len(cfg.Projects) > 0 {
		post.Restricted = true
		post.Projects = cfg.Projects
	}

	enrollCtx, cancel := context.WithTimeout(ctx, enrollTimeout)
	defer cancel()

	err = conn.CreateCertificate(enrollCtx, post)
	// Already in the trust store is a success: the pair just generated is the
	// one the daemon has.
	if err != nil && !trusted(err) {
		return nil, fmt.Errorf("redeeming the trust token: %w", err)
	}

	err = writePair(cfg.DataDir, certPEM, keyPEM)
	if err != nil {
		return nil, err
	}

	// Reading /1.0 is lazy, so this connection needs no redial.
	return conn, nil
}

// dial builds the connection. The server certificate is not pinned.
func dial(cfg Config, certPEM, keyPEM []byte) (*iclient.Connection, error) {
	if cfg.URL == "" {
		return nil, errors.New("no Incus endpoint")
	}

	return iclient.NewConnection(&iclient.ConfigRemoteInfo{
		Name:               cfg.Name,
		UserAgent:          cfg.agent(),
		Addrs:              []string{cfg.URL},
		Protocol:           "incus",
		ClientCert:         string(certPEM),
		ClientKey:          string(keyPEM),
		InsecureSkipVerify: true,
	})
}

// remote connects as an entry in the Incus CLI's own configuration: the
// developer's path, tried last because it answers with whatever identity a home
// directory holds.
func remote(cfg Config) (*iclient.Connection, error) {
	conf, err := iclient.ReadConfig("")
	if err != nil {
		return nil, fmt.Errorf("reading the Incus configuration: %w", err)
	}

	info, err := conf.RemoteInfos(cfg.Remote)
	if err != nil {
		return nil, fmt.Errorf("resolving the Incus remote: %w", err)
	}

	if cfg.agent() != "" {
		info.UserAgent = cfg.agent()
	}

	return iclient.NewConnection(info)
}

// token returns the trust token, from configuration or from the file a secret
// is mounted as. Empty is not an error: there may be a certificate instead.
func token(cfg Config) (string, error) {
	if cfg.Token != "" {
		return cfg.Token, nil
	}

	if cfg.SecretsDir == "" {
		return "", nil
	}

	data, err := os.ReadFile(filepath.Join(cfg.SecretsDir, TokenFile))
	if os.IsNotExist(err) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("reading the trust token: %w", err)
	}

	// A mounted secret that is there but empty meant to supply one, so it fails
	// rather than falling through to a path that connects as somebody else.
	out := strings.TrimSpace(string(data))
	if out == "" {
		return "", errors.New("the trust token file is empty")
	}

	return out, nil
}

// readPair reads a certificate and its key. A missing file reports os.IsNotExist
// so the caller can tell "not enrolled yet" from "unreadable".
func readPair(certPath, keyPath string) ([]byte, []byte, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}

	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}

	return certPEM, keyPEM, nil
}

// writePair persists an enrolled pair, key first: a certificate without its key
// is what the next run would try to connect with.
func writePair(dir string, certPEM, keyPEM []byte) error {
	if dir == "" {
		return errors.New("no data directory to persist the certificate in")
	}

	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		return err
	}

	err = os.WriteFile(filepath.Join(dir, KeyFile), keyPEM, 0o600)
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(dir, CertFile), certPEM, 0o600)
}

// trusted reports whether the daemon refused a token because it already trusts
// this certificate. By message, because Incus answers both with a plain 400.
func trusted(err error) bool {
	msg := err.Error()

	return strings.Contains(msg, "already trusted") || strings.Contains(msg, "already exists")
}

// generate makes a self-signed client certificate.
func generate(name string) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}

	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		nil
}
