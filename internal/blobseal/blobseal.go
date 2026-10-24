// Package blobseal seals the device blobs at rest (docs/design.md section 7). A blob is the
// adapter's record and holds the console PIN, which the console needs in clear, so it is
// encrypted rather than hashed: AES-256-GCM under PARENTAL_BLOB_KEY, the device's namespace
// and ref as additional data so a sealed blob cannot be moved to another row.
//
// A sealed blob is itself a JSON object, so it fits the column as it is:
//
//	{"sealed":"aes-256-gcm/v1","kid":"<key id>","nonce":"<b64>","ciphertext":"<b64>"}
//
// kid names the key (a hash of it, never the key), so a later rotation can tell which key
// a row was sealed under. The top-level "sealed" member is reserved: an adapter blob may not
// carry one.
package blobseal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Scheme is the only one there is.
const Scheme = "aes-256-gcm/v1"

type Key struct {
	ID   string
	aead cipher.AEAD
}

type envelope struct {
	Sealed     string `json:"sealed"`
	KID        string `json:"kid"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

// Parse takes 32 bytes in standard base64 (openssl rand -base64 32).
func Parse(b64 string) (*Key, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(raw) != 32 {
		return nil, errors.New("blobseal: the key must be 32 bytes in base64 (openssl rand -base64 32)")
	}
	block, _ := aes.NewCipher(raw)
	aead, _ := cipher.NewGCM(block)
	sum := sha256.Sum256(append([]byte("openpak blob key id\x00"), raw...))
	return &Key{ID: hex.EncodeToString(sum[:8]), aead: aead}, nil
}

func aad(namespace, ref string) []byte {
	return []byte("openpak parental device\x00" + namespace + "\x00" + ref)
}

// Seal is the sealed blob for one device's plaintext.
func (k *Key) Seal(namespace, ref string, plain []byte) []byte {
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	b, _ := json.Marshal(envelope{Sealed: Scheme, KID: k.ID, Nonce: nonce, Ciphertext: k.aead.Seal(nil, nonce, plain, aad(namespace, ref))})
	return b
}

// IsSealed reports whether a blob is a sealed one (has a top-level "sealed" member).
func IsSealed(b []byte) bool {
	var top map[string]json.RawMessage
	return json.Unmarshal(b, &top) == nil && top["sealed"] != nil
}

// Open is a blob's plaintext: a sealed blob opened with k (nil k cannot open one), a blob
// in clear returned as it is.
func Open(k *Key, namespace, ref string, b []byte) ([]byte, error) {
	if !IsSealed(b) {
		return b, nil
	}
	var e envelope
	err := json.Unmarshal(b, &e)
	switch {
	case err != nil:
		return nil, errors.New("blobseal: corrupt sealed blob")
	case e.Sealed != Scheme:
		return nil, fmt.Errorf("blobseal: unknown scheme %q", e.Sealed)
	case k == nil:
		return nil, errors.New("blobseal: the blob is sealed and no PARENTAL_BLOB_KEY is configured")
	case e.KID != k.ID:
		return nil, fmt.Errorf("blobseal: sealed under key %s, the configured key is %s", e.KID, k.ID)
	}
	plain, err := k.aead.Open(nil, e.Nonce, e.Ciphertext, aad(namespace, ref))
	if err != nil {
		return nil, errors.New("blobseal: the blob does not open with the configured key")
	}
	return plain, nil
}
