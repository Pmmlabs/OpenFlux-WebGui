package exitmgr

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/flynn/noise"

	"openflux/transport"
)

// Regression test for a real bug: BuildTransport used to wrap encryption
// OUTSIDE the codec (backend -> codec -> encryption), the reverse of
// main.go's wiring (backend -> encryption -> codec). A panel client's Noise
// handshake frame arrived at the exit's outermost BatchedTransport, which
// can't decode it (it's not a batch frame) and silently drops it -- the
// handshake never reaches EncryptedTransport underneath, so it never
// completes. This asserts the layering directly instead of relying on a
// live handshake, so it doesn't need network.
func TestBuildTransportWrapsEncryptionUnderTheCodec(t *testing.T) {
	key, err := transport.GenerateStaticKey()
	if err != nil {
		t.Fatalf("generate static key: %v", err)
	}

	cfg := ClientConfig{
		ID:        "c1",
		Transport: "yandex",
		URL:       "https://disk.yandex.com/i/does-not-matter",
	}

	trans, err := BuildTransport(cfg, transport.DefaultConfig(), key)
	if err != nil {
		t.Fatalf("BuildTransport: %v", err)
	}

	batched, ok := trans.(*transport.BatchedTransport)
	if !ok {
		t.Fatalf("outermost transport is %T, want *transport.BatchedTransport (codec must be outermost)", trans)
	}
	if _, ok := batched.Transport.(*transport.EncryptedTransport); !ok {
		t.Fatalf("codec wraps %T, want *transport.EncryptedTransport directly underneath "+
			"(encryption must sit on the raw transport, under the codec, like main.go wires it)", batched.Transport)
	}
}

func TestBuildTransportWrapsLegacyCodecOverEncryption(t *testing.T) {
	key, err := transport.GenerateStaticKey()
	if err != nil {
		t.Fatalf("generate static key: %v", err)
	}

	cfg := ClientConfig{
		ID:        "c1",
		Transport: "yandex",
		URL:       "https://disk.yandex.com/i/does-not-matter",
		Codec:     "legacy",
	}

	trans, err := BuildTransport(cfg, transport.DefaultConfig(), key)
	if err != nil {
		t.Fatalf("BuildTransport: %v", err)
	}

	compressed, ok := trans.(*transport.CompressedTransport)
	if !ok {
		t.Fatalf("outermost transport is %T, want *transport.CompressedTransport", trans)
	}
	if _, ok := compressed.Transport.(*transport.EncryptedTransport); !ok {
		t.Fatalf("codec wraps %T, want *transport.EncryptedTransport directly underneath", compressed.Transport)
	}
}

// Without a panel key (--panel-key-file not given) the client tunnel stays
// plaintext: the codec must wrap the raw backend directly. A psk_file alone
// switches it to the PSK-only AES-256-GCM transport — over the codec, the
// v1 layering, so official client can interoperate.
func TestBuildTransportPlaintextWithoutPanelKey(t *testing.T) {
	cfg := ClientConfig{
		ID:        "c1",
		Transport: "yandex",
		URL:       "https://disk.yandex.com/i/does-not-matter",
	}

	trans, err := BuildTransport(cfg, transport.DefaultConfig(), noise.DHKey{})
	if err != nil {
		t.Fatalf("BuildTransport: %v", err)
	}
	batched, ok := trans.(*transport.BatchedTransport)
	if !ok {
		t.Fatalf("outermost transport is %T, want *transport.BatchedTransport", trans)
	}
	if _, ok := batched.Transport.(*transport.EncryptedTransport); ok {
		t.Fatal("encryption configured without a panel key")
	}

	pskPath := filepath.Join(t.TempDir(), "psk.txt")
	if err := os.WriteFile(pskPath, []byte("a sufficiently long shared secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.PSKFile = pskPath
	trans, err = BuildTransport(cfg, transport.DefaultConfig(), noise.DHKey{})
	if err != nil {
		t.Fatalf("BuildTransport with psk and no panel key: %v", err)
	}
	pskTrans, ok := trans.(*transport.PSKTransport)
	if !ok {
		t.Fatalf("outermost transport is %T, want *transport.PSKTransport (PSK-only wraps the codec)", trans)
	}
	if _, ok := pskTrans.Transport.(*transport.BatchedTransport); !ok {
		t.Fatalf("PSK-only wraps %T, want *transport.BatchedTransport directly underneath (v1 layering)", pskTrans.Transport)
	}
}
