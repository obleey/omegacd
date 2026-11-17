package source

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// The files GenerateCerts writes: the Source Server needs ca.pem and server.*, the Control Plane ca.pem and client.*.
const (
	caFile     = "ca.pem"
	serverCert = "server.pem"
	serverKey  = "server.key"
	clientCert = "client.pem"
	clientKey  = "client.key"
)

// GenerateCerts writes a new CA and the server and client certificates it signs into dir (Q45). hosts are the
// names and addresses the Control Plane dials the Source Server by. The CA's key is not kept: nobody can issue
// another certificate it trusts, and new certificates mean running this again. It never overwrites a file.
func GenerateCerts(dir string, hosts []string) error {
	for _, f := range []string{caFile, serverCert, serverKey, clientCert, clientKey} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return fmt.Errorf("%s already exists; remove the old certificates first", filepath.Join(dir, f))
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil { // coverage: unreachable, crypto/rand doesn't fail
		return err
	}
	ca := template("omegacd source CA")
	ca.IsCA, ca.BasicConstraintsValid, ca.KeyUsage = true, true, x509.KeyUsageCertSign
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil { // coverage: unreachable, a valid template and a key just made
		return err
	}
	if err := writePEM(filepath.Join(dir, caFile), "CERTIFICATE", caDER, 0o644); err != nil {
		return err
	}

	srv := template("omegacd source server")
	srv.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			srv.IPAddresses = append(srv.IPAddresses, ip)
		} else {
			srv.DNSNames = append(srv.DNSNames, h)
		}
	}
	cli := template("omegacd control plane")
	cli.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	for _, c := range []struct {
		t          *x509.Certificate
		cert, priv string
	}{{srv, serverCert, serverKey}, {cli, clientCert, clientKey}} {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil { // coverage: unreachable, crypto/rand doesn't fail
			return err
		}
		der, err := x509.CreateCertificate(rand.Reader, c.t, ca, &key.PublicKey, caKey)
		if err != nil { // coverage: unreachable, a valid template and keys just made
			return err
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil { // coverage: unreachable, an ECDSA P-256 key always marshals
			return err
		}
		if err := writePEM(filepath.Join(dir, c.cert), "CERTIFICATE", der, 0o644); err != nil {
			return err
		}
		if err := writePEM(filepath.Join(dir, c.priv), "PRIVATE KEY", keyDER, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func template(cn string) *x509.Certificate {
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
}

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	err = pem.Encode(f, &pem.Block{Type: typ, Bytes: der})
	return errors.Join(err, f.Close())
}

func caPool(dir string) (*x509.CertPool, error) {
	b, err := os.ReadFile(filepath.Join(dir, caFile))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("%s: no certificate", filepath.Join(dir, caFile))
	}
	return pool, nil
}

// ServerTLS accepts only clients with a certificate from the CA in dir: the Control Plane.
func ServerTLS(dir string) (*tls.Config, error) {
	pool, err := caPool(dir)
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, serverCert), filepath.Join(dir, serverKey))
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, ClientCAs: pool,
		ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13}, nil
}

// ClientTLS trusts only a Source Server with a certificate from the CA in dir, for serverName.
func ClientTLS(dir, serverName string) (*tls.Config, error) {
	pool, err := caPool(dir)
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, clientCert), filepath.Join(dir, clientKey))
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, ServerName: serverName,
		MinVersion: tls.VersionTLS13}, nil
}
