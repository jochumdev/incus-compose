package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"strings"
	"time"
)

// levelTrace is below slog's Debug, for the per-event and per-check lines that
// would otherwise drown a debug session.
const levelTrace = slog.Level(-8)

// logLevel maps the two verbosity flags to a level; trace implies debug.
func logLevel(debug, trace bool) slog.Level {
	switch {
	case trace:
		return levelTrace
	case debug:
		return slog.LevelDebug
	default:
		return slog.LevelInfo
	}
}

// newLogHandler builds the daemon's handler, naming levelTrace so it does not
// print as slog's "DEBUG-4".
func newLogHandler(w io.Writer, level slog.Level) slog.Handler {
	return slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key != slog.LevelKey {
				return a
			}

			if lvl, ok := a.Value.Any().(slog.Level); ok && lvl == levelTrace {
				a.Value = slog.StringValue("TRACE")
			}

			return a
		},
	})
}

// loggerKey is the key the daemon's logger travels under.
type loggerKey struct{}

func withLogger(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, log)
}

func logger(ctx context.Context) *slog.Logger {
	log, ok := ctx.Value(loggerKey{}).(*slog.Logger)
	if !ok {
		return slog.Default()
	}

	return log
}

// procNetRoute is where the kernel lists the routing table.
const procNetRoute = "/proc/net/route"

func hasDefaultRoute(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	// [1:] skips the header row.
	for _, line := range strings.Split(string(data), "\n")[1:] {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[1] == "00000000" {
			return true
		}
	}

	return false
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// generateClientCert returns a PEM-encoded ECDSA P-384 key pair and self-signed cert.
func generateClientCert() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("ecdsa key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("serial: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "ic-healthd"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("creating cert: %w", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling key: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}
