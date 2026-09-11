package certs

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	// pemExt is the extension of the file holding a host's certificate and key.
	pemExt = ".pem"
	// maxFileName is the longest file name common filesystems allow.
	maxFileName = 255
)

// fileName is the file a host's certificate and key are kept in: the host name itself
// where that fits, so the directory reads at a glance, with a wildcard spelled out. DNS
// allows names of 253 bytes, which with the extension is too long for a file name, so a
// name that long is hashed instead.
func fileName(name string) string {
	base := strings.Replace(name, "*", "_wildcard", 1)
	if len(base)+len(pemExt) > maxFileName {
		sum := sha256.Sum256([]byte(name))
		base = hex.EncodeToString(sum[:])
	}
	return base + pemExt
}
