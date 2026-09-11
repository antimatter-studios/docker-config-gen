// Package certs issues the per-host TLS certificates the proxy serves HTTPS with.
//
// They are signed by a local certificate authority that the orchestrator creates and
// trusts on the developer's machine (ddt does this on install) and mounts into this
// container read-only. No public CA is involved: the names are local development hosts,
// which no public CA would issue for anyway.
package certs

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// CAFile and CAKeyFile are the CA certificate and its private key, as PEM, in the
	// directory given to Load.
	CAFile    = "ca.crt"
	CAKeyFile = "ca.key"

	// Lifetime is the longest validity Apple platforms accept for a TLS server
	// certificate, including one from a CA the user installed themselves. Safari and
	// iOS reject anything longer, whichever CA signed it.
	Lifetime = 825 * 24 * time.Hour

	// RenewBefore is how much validity a certificate must have left to be kept.
	RenewBefore = 30 * 24 * time.Hour
)

// ErrNoCA means the CA directory holds neither the certificate nor the key, so HTTPS
// has not been set up.
var ErrNoCA = errors.New("no CA certificate and key")

// Issuer keeps one certificate per host name in a directory, issuing and renewing them
// with its CA.
type Issuer struct {
	ca    *x509.Certificate
	caKey crypto.Signer
	roots *x509.CertPool
	dir   string

	// now is the clock, replaceable in tests.
	now func() time.Time

	// mu serialises Certificate: two updates issuing the same host at once could
	// otherwise leave one's key beside the other's certificate.
	mu sync.Mutex
	// refused holds the hosts already logged as impossible, so each is logged once
	// rather than on every Docker event.
	refused map[string]bool
}

// Load reads the CA from caDir and returns an Issuer that writes into dir. It returns
// ErrNoCA when caDir has neither file, which callers treat as "HTTPS is not set up"
// rather than as a failure.
func Load(caDir, dir string) (*Issuer, error) {
	certPEM, certErr := os.ReadFile(filepath.Join(caDir, CAFile))
	keyPEM, keyErr := os.ReadFile(filepath.Join(caDir, CAKeyFile))
	if errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist) {
		return nil, ErrNoCA
	}
	if certErr != nil {
		return nil, fmt.Errorf("reading the CA certificate: %w", certErr)
	}
	if keyErr != nil {
		return nil, fmt.Errorf("reading the CA key: %w", keyErr)
	}

	// X509KeyPair also checks that the key belongs to the certificate.
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("the CA certificate and key: %w", err)
	}
	ca, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parsing the CA certificate: %w", err)
	}
	if !ca.IsCA {
		return nil, errors.New("the CA certificate is not a CA certificate")
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("the CA key cannot sign")
	}

	roots := x509.NewCertPool()
	roots.AddCert(ca)

	return &Issuer{
		ca:      ca,
		caKey:   signer,
		roots:   roots,
		dir:     dir,
		now:     time.Now,
		refused: map[string]bool{},
	}, nil
}

// Certificate returns the certificate and key files for host, issuing them first when
// there is no current certificate: none yet, one with less than RenewBefore left, one
// for another name, or one from a different CA. changed reports that the files were
// written just now, which nginx only notices on a reload. ok is false when host cannot
// have a certificate from this CA; the host then stays HTTP-only.
func (i *Issuer) Certificate(host string) (certFile, keyFile string, changed, ok bool) {
	i.mu.Lock()
	defer i.mu.Unlock()

	name := strings.ToLower(host)
	if !usableName(name) {
		i.refuse(host, "not a host name a certificate can carry")
		return "", "", false, false
	}

	base := strings.Replace(name, "*", "_wildcard", 1)
	certFile = filepath.Join(i.dir, base+".crt")
	keyFile = filepath.Join(i.dir, base+".key")

	if i.current(name, certFile, keyFile) {
		return certFile, keyFile, false, true
	}
	if err := i.issue(name, certFile, keyFile); err != nil {
		i.refuse(host, err.Error())
		return "", "", false, false
	}
	delete(i.refused, host)
	return certFile, keyFile, true, true
}

// current reports whether the files hold a matching certificate and key, for name,
// from this CA, with more than RenewBefore left.
func (i *Issuer) current(name, certFile, keyFile string) bool {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return false
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return false
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return false
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return false
	}
	if i.verify(leaf, name) != nil {
		return false
	}
	return leaf.NotAfter.Sub(i.now()) > RenewBefore
}

// issue writes a new key and certificate for name.
func (i *Issuer) issue(name, certFile, keyFile string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generating a key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generating a serial number: %w", err)
	}

	// Backdated an hour, so a client whose clock is slightly behind still accepts it.
	notBefore := i.now().Add(-time.Hour)
	notAfter := notBefore.Add(Lifetime)
	if notAfter.After(i.ca.NotAfter) {
		notAfter = i.ca.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		DNSNames:              []string{name},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, i.ca, &key.PublicKey, i.caKey)
	if err != nil {
		return fmt.Errorf("signing: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return fmt.Errorf("parsing the issued certificate: %w", err)
	}
	// A CA restricted to certain names can still sign any name, and clients then reject
	// the result. Check it the way they will, and write nothing if it fails.
	if err := i.verify(leaf, name); err != nil {
		return fmt.Errorf("the CA cannot vouch for it: %w", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("encoding the key: %w", err)
	}
	if err := writeFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return fmt.Errorf("writing the key: %w", err)
	}
	if err := writeFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return fmt.Errorf("writing the certificate: %w", err)
	}
	return nil
}

// verify checks leaf the way a client would: signed by this CA, valid now, for name,
// for server use, and within any names the CA is restricted to.
func (i *Issuer) verify(leaf *x509.Certificate, name string) error {
	_, err := leaf.Verify(x509.VerifyOptions{
		Roots: i.roots,
		// A wildcard is not a name a client connects to, so check one it covers.
		DNSName:     strings.Replace(name, "*", "wildcard-check", 1),
		CurrentTime: i.now(),
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	return err
}

// refuse logs, once per host, why host gets no certificate. The caller holds i.mu.
func (i *Issuer) refuse(host, reason string) {
	if i.refused[host] {
		return
	}
	i.refused[host] = true
	log.Printf("No HTTPS for %q: %s", host, reason)
}

// usableName reports whether name can go in a certificate: a DNS name, optionally with
// one leading wildcard label. nginx's server_name also takes regular expressions and
// other forms, which no certificate can carry.
func usableName(name string) bool {
	rest := strings.TrimPrefix(name, "*.")
	if rest == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(rest, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// writeFile replaces path atomically, so nginx never reads a half-written file.
func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
