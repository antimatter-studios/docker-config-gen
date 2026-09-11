package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newCA writes a CA into a fresh directory, restricted to the permitted names when any
// are given, the way ddt restricts its CA to development domains.
func newCA(t *testing.T, permitted ...string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:                big.NewInt(1),
		Subject:                     pkix.Name{CommonName: "test CA"},
		NotBefore:                   time.Now().Add(-time.Hour),
		NotAfter:                    time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomains:         permitted,
		PermittedDNSDomainsCritical: len(permitted) > 0,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writePEM(t, filepath.Join(dir, CAFile), "CERTIFICATE", der)
	writePEM(t, filepath.Join(dir, CAKeyFile), "PRIVATE KEY", keyDER)
	return dir
}

func writePEM(t *testing.T, path, kind string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, caDir, dir string) *Issuer {
	t.Helper()
	i, err := Load(caDir, dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return i
}

func leaf(t *testing.T, certFile string) *x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatalf("%s holds no PEM", certFile)
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// trusts reports whether a client that trusts the CA in caDir accepts cert for name.
func trusts(t *testing.T, caDir string, cert *x509.Certificate, name string) error {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(leaf(t, filepath.Join(caDir, CAFile)))
	_, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: name})
	return err
}

func TestIssuesACertificateAClientAccepts(t *testing.T) {
	caDir := newCA(t, "localhost")
	i := load(t, caDir, t.TempDir())

	certFile, keyFile, changed, ok := i.Certificate("app.localhost")
	if !ok || !changed {
		t.Fatalf("ok=%v changed=%v, want both true", ok, changed)
	}
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		t.Fatalf("the certificate and key are not a pair nginx can load: %v", err)
	}
	c := leaf(t, certFile)
	if err := trusts(t, caDir, c, "app.localhost"); err != nil {
		t.Errorf("a client trusting the CA rejects it: %v", err)
	}
	if got := c.NotAfter.Sub(c.NotBefore); got > Lifetime {
		t.Errorf("valid for %v; Apple platforms reject more than %v", got, Lifetime)
	}

	fi, err := os.Stat(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("key is mode %o, want 600", perm)
	}
}

func TestKeepsACurrentCertificate(t *testing.T) {
	i := load(t, newCA(t), t.TempDir())
	certFile, _, _, _ := i.Certificate("app.localhost")
	first := leaf(t, certFile).SerialNumber

	_, _, changed, ok := i.Certificate("app.localhost")
	if !ok || changed {
		t.Fatalf("ok=%v changed=%v, want the current certificate kept", ok, changed)
	}
	if leaf(t, certFile).SerialNumber.Cmp(first) != 0 {
		t.Error("a current certificate was replaced")
	}
}

func TestRenewsACertificateNearExpiry(t *testing.T) {
	i := load(t, newCA(t), t.TempDir())
	certFile, _, _, _ := i.Certificate("app.localhost")
	first := leaf(t, certFile).SerialNumber

	// A day inside the renewal window.
	i.now = func() time.Time { return time.Now().Add(Lifetime - RenewBefore + 24*time.Hour) }
	_, _, changed, ok := i.Certificate("app.localhost")
	if !ok || !changed {
		t.Fatalf("ok=%v changed=%v, want a renewed certificate", ok, changed)
	}
	if leaf(t, certFile).SerialNumber.Cmp(first) == 0 {
		t.Error("the certificate was not replaced")
	}
}

// A certificate from a CA that has since been replaced is useless to clients that
// trust only the new one.
func TestReissuesWhenTheCAChanges(t *testing.T) {
	dir := t.TempDir()
	load(t, newCA(t), dir).Certificate("app.localhost")

	caDir := newCA(t)
	certFile, _, changed, ok := load(t, caDir, dir).Certificate("app.localhost")
	if !ok || !changed {
		t.Fatalf("ok=%v changed=%v, want a certificate from the new CA", ok, changed)
	}
	if err := trusts(t, caDir, leaf(t, certFile), "app.localhost"); err != nil {
		t.Errorf("not signed by the new CA: %v", err)
	}
}

// The CA is restricted to development names, so a certificate for anything else would
// be rejected by every client. It must not be written at all.
func TestRefusesNamesOutsideTheCA(t *testing.T) {
	dir := t.TempDir()
	i := load(t, newCA(t, "localhost"), dir)

	if _, _, _, ok := i.Certificate("app.example.com"); ok {
		t.Fatal("issued a certificate for a name the CA may not vouch for")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("files were left behind: %v", entries)
	}
}

func TestRefusesNamesACertificateCannotCarry(t *testing.T) {
	i := load(t, newCA(t), t.TempDir())
	for _, host := range []string{
		"",
		`~^app\.localhost$`, // nginx regular expression
		"app localhost",
		"app_1.localhost",
		"*.*.localhost",
		".localhost",
		"-app.localhost",
	} {
		if _, _, _, ok := i.Certificate(host); ok {
			t.Errorf("issued a certificate for %q", host)
		}
	}
}

func TestWildcardHost(t *testing.T) {
	caDir := newCA(t, "localhost")
	i := load(t, caDir, t.TempDir())

	certFile, _, _, ok := i.Certificate("*.app.localhost")
	if !ok {
		t.Fatal("no certificate for a wildcard host")
	}
	if strings.Contains(filepath.Base(certFile), "*") {
		t.Errorf("file name %q contains a wildcard", certFile)
	}
	if err := trusts(t, caDir, leaf(t, certFile), "api.app.localhost"); err != nil {
		t.Errorf("does not cover a name under the wildcard: %v", err)
	}
}

func TestLoadWithoutACA(t *testing.T) {
	if _, err := Load(t.TempDir(), t.TempDir()); !errors.Is(err, ErrNoCA) {
		t.Errorf("err = %v, want ErrNoCA", err)
	}
}

// A certificate whose key is from another CA is a broken setup, not an absent one.
func TestLoadRefusesAMismatchedKey(t *testing.T) {
	caDir := newCA(t)
	other := newCA(t)
	data, err := os.ReadFile(filepath.Join(other, CAKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(caDir, CAKeyFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(caDir, t.TempDir())
	if err == nil || errors.Is(err, ErrNoCA) {
		t.Errorf("err = %v, want a load failure", err)
	}
}

// A certificate and key kept as two files can disagree: if the second write fails, the
// new key sits beside the old certificate and nginx refuses the pair. One file holding
// both is replaced by a single rename, so it never can.
func TestCertificateAndKeyAreOneFile(t *testing.T) {
	i := load(t, newCA(t), t.TempDir())
	certFile, keyFile, _, ok := i.Certificate("app.localhost")
	if !ok {
		t.Fatal("no certificate")
	}
	if certFile != keyFile {
		t.Errorf("the certificate (%s) and key (%s) are separate files", certFile, keyFile)
	}
}

// A DNS name may be 253 bytes but a file name only 255, and the extension has to fit
// too. A host that long must still get its certificate.
func TestLongHostName(t *testing.T) {
	label := strings.Repeat("a", 63)
	host := strings.Join([]string{label, label, label, strings.Repeat("b", 51), "localhost"}, ".")
	if len(host) != 253 {
		t.Fatalf("test host is %d bytes, want 253", len(host))
	}
	caDir := newCA(t)
	certFile, _, _, ok := load(t, caDir, t.TempDir()).Certificate(host)
	if !ok {
		t.Fatal("no certificate for a 253-byte host name")
	}
	if n := len(filepath.Base(certFile)); n > 255 {
		t.Errorf("file name is %d bytes", n)
	}
	if err := trusts(t, caDir, leaf(t, certFile), host); err != nil {
		t.Errorf("the certificate does not cover the host: %v", err)
	}
}
