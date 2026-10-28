// Package identity owns the long-term key material that defines a
// machine on the LAN: a Noise static key for key agreement and an
// Ed25519 key for signing presence announcements. Borrowed from
// bitchat: identity is a key fingerprint, never a nickname.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/flynn/noise"
)

// Identity is persisted to disk as JSON with 0600 permissions.
type Identity struct {
	NoisePrivate string `json:"noise_private"`
	NoisePublic  string `json:"noise_public"`
	SignPrivate  string `json:"sign_private"`
	SignPublic   string `json:"sign_public"`
}

var cipherSuite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256)

// CipherSuite exposes the shared Noise cipher suite.
func CipherSuite() noise.CipherSuite { return cipherSuite }

// LoadOrCreate reads the identity file, generating fresh keys on
// first run.
func LoadOrCreate(dataDir string) (*Identity, error) {
	path := filepath.Join(dataDir, "identity.json")
	raw, err := os.ReadFile(path)
	if err == nil {
		var id Identity
		if err := json.Unmarshal(raw, &id); err != nil {
			return nil, fmt.Errorf("corrupt identity file: %w", err)
		}
		if _, err := id.NoiseKeypair(); err != nil {
			return nil, err
		}
		return &id, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	kp, err := cipherSuite.GenerateKeypair(rand.Reader)
	if err != nil {
		return nil, err
	}
	signPub, signPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	id := &Identity{
		NoisePrivate: base64.StdEncoding.EncodeToString(kp.Private),
		NoisePublic:  base64.StdEncoding.EncodeToString(kp.Public),
		SignPrivate:  base64.StdEncoding.EncodeToString(signPriv),
		SignPublic:   base64.StdEncoding.EncodeToString(signPub),
	}
	out, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return nil, err
	}
	return id, nil
}

// NoiseKeypair returns the flynn/noise keypair.
func (id *Identity) NoiseKeypair() (noise.DHKey, error) {
	priv, err := base64.StdEncoding.DecodeString(id.NoisePrivate)
	if err != nil {
		return noise.DHKey{}, err
	}
	pub, err := base64.StdEncoding.DecodeString(id.NoisePublic)
	if err != nil {
		return noise.DHKey{}, err
	}
	return noise.DHKey{Private: priv, Public: pub}, nil
}

// NoisePublicBytes decodes the Noise public key.
func (id *Identity) NoisePublicBytes() []byte {
	b, _ := base64.StdEncoding.DecodeString(id.NoisePublic)
	return b
}

// SignPublicBytes decodes the Ed25519 public key.
func (id *Identity) SignPublicBytes() []byte {
	b, _ := base64.StdEncoding.DecodeString(id.SignPublic)
	return b
}

// Sign signs a message with the Ed25519 key.
func (id *Identity) Sign(msg []byte) []byte {
	priv, err := base64.StdEncoding.DecodeString(id.SignPrivate)
	if err != nil {
		return nil
	}
	return ed25519.Sign(ed25519.PrivateKey(priv), msg)
}

// PeerID derives the stable short peer id from a Noise public key:
// the first 8 bytes of its SHA-256 fingerprint, hex encoded.
func PeerID(noisePub []byte) string {
	sum := sha256.Sum256(noisePub)
	return hex.EncodeToString(sum[:8])
}

// Fingerprint is the full hex SHA-256 of a Noise public key.
func Fingerprint(noisePub []byte) string {
	sum := sha256.Sum256(noisePub)
	return hex.EncodeToString(sum[:])
}

const base32Alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Code renders a short human-comparable verification code for a
// Noise public key. Both sides compute it from the same key, so
// reading it aloud over a desk is enough to pin the right peer.
func Code(noisePub []byte) string {
	sum := sha256.Sum256(noisePub)
	// Take 40 bits -> 8 base32 chars, grouped XXXX-XXXX.
	var v uint64
	for i := 0; i < 5; i++ {
		v = v<<8 | uint64(sum[i])
	}
	out := make([]byte, 0, 9)
	for i := 7; i >= 0; i-- {
		out = append(out, base32Alphabet[(v>>uint(5*i))&31])
		if i == 4 {
			out = append(out, '-')
		}
	}
	return string(out)
}

// MyPeerID is this machine's peer id.
func (id *Identity) MyPeerID() string { return PeerID(id.NoisePublicBytes()) }

// MyCode is this machine's verification code.
func (id *Identity) MyCode() string { return Code(id.NoisePublicBytes()) }

// Verify checks an Ed25519 signature.
func Verify(signPub, msg, sig []byte) bool {
	if len(signPub) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(signPub), msg, sig)
}
