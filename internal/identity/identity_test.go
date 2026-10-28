package identity

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestPeerIDAndCodeAreStableAndKeyBound(t *testing.T) {
	pub := []byte("chuanshu-test-public-key-32byte!")
	sum := sha256.Sum256(pub)
	if got, want := PeerID(pub), hex.EncodeToString(sum[:8]); got != want {
		t.Fatalf("PeerID = %s, want %s", got, want)
	}
	if Code(pub) != Code(pub) {
		t.Fatal("Code is not deterministic")
	}
	if Code(pub) == Code([]byte("a-different-key")) {
		t.Fatal("Code collided across keys")
	}
	if len(Code(pub)) != 9 {
		t.Fatalf("Code length = %d, want 9 (XXXX-XXXX)", len(Code(pub)))
	}
}

func TestLoadOrCreatePersistsAndSigns(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("identity file mode = %o, want 600", info.Mode().Perm())
	}
	again, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again.MyPeerID() != id.MyPeerID() {
		t.Fatal("peer id changed across reload")
	}
	msg := []byte("announce")
	sig := id.Sign(msg)
	if !ed25519.Verify(ed25519.PublicKey(id.SignPublicBytes()), msg, sig) {
		t.Fatal("signature does not verify")
	}
	if Verify(id.SignPublicBytes(), []byte("tampered"), sig) {
		t.Fatal("signature verified for tampered message")
	}
}
