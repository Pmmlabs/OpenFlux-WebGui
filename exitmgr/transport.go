package exitmgr

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/flynn/noise"

	"openflux/transport"
	"openflux/transport/cupsonline"
	"openflux/transport/mailru"
	"openflux/transport/oneme"
	"openflux/transport/yandex"
)

// BuildTransport constructs the full transport stack for one client
// (backend -> encryption -> codec), mirroring main.go's single-client
// wiring so a client added through the panel behaves identically to one
// started via CLI flags. Every client shares the panel's own static key
// (staticKey, loaded once by runExitPanel); cfg.PSKFile optionally closes
// this one client to strangers who don't have the shared secret. A zero
// staticKey (no --panel-key-file) leaves the tunnel plaintext, matching
// the CLI's opt-in encryption.
func BuildTransport(cfg ClientConfig, base transport.TransportConfig, staticKey noise.DHKey) (transport.Transport, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	var inner transport.Transport
	switch cfg.Transport {
	case "yandex":
		inner = yandex.NewYandexDocsTransport(cfg.URL, base)
	case "vyandex":
		inner = yandex.NewYandexVolgaTransport(cfg.URL, base)
	case "oneme":
		uid, err := strconv.ParseInt(cfg.MaxUid, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("max_uid: %w", err)
		}
		inner = oneme.NewOneMeTransport(true, cfg.MaxToken, uid, base)
	case "cupsonline":
		inner = cupsonline.NewCupsonlineTransport(cfg.URL, base, false)
	case "mailru":
		inner = mailru.NewMailruDocsTransport(cfg.URL, base)
	default:
		return nil, fmt.Errorf("unknown transport %q", cfg.Transport)
	}

	var pskSecret string
	if cfg.PSKFile != "" {
		secretBytes, err := os.ReadFile(cfg.PSKFile)
		if err != nil {
			return nil, fmt.Errorf("read psk file: %w", err)
		}
		pskSecret = strings.TrimSpace(string(secretBytes))
	}

	// Encryption layering, mirroring main.go. With a panel key
	// (--panel-key-file) it is the Noise NKpsk0 transport sitting on the raw
	// transport, under the codec: one AEAD covers a whole compressed batch.
	// With only a client psk_file it is the PSK-only AES-256-GCM transport
	// wrapping the codec instead (the v1 --encryption-key-file layering,
	// wire = batch(v1 frame)), so official clients can interoperate
	// and the PSK works without --panel-key-file.
	if staticKey.Private != nil {
		var psk []byte
		if pskSecret != "" {
			var err error
			psk, err = transport.DerivePSK(pskSecret)
			if err != nil {
				return nil, fmt.Errorf("psk file: %w", err)
			}
		}
		enc, err := transport.NewEncryptedTransport(inner, transport.EncryptedConfig{
			Initiator: false,
			StaticKey: staticKey,
			PSK:       psk,
		})
		if err != nil {
			return nil, fmt.Errorf("configure encryption: %w", err)
		}
		inner = enc
	}

	// App-layer codec, the same default and layering as main.go.
	if cfg.Codec == "legacy" {
		inner = transport.NewCompressedTransport(inner)
	} else {
		inner = transport.NewBatchedTransport(inner)
	}

	// PSK-only sits over the codec (v1 layering), after it.
	if staticKey.Private == nil && pskSecret != "" {
		// The PSK-only KDF salt must match the client's, which derives it
		// from its --url (or transport type), like the v1 flag did.
		context := cfg.Transport
		if cfg.URL != "" {
			context = cfg.URL
		}
		enc, err := transport.NewPSKTransport(inner, pskSecret, context, false)
		if err != nil {
			return nil, fmt.Errorf("configure encryption: %w", err)
		}
		inner = enc
	}

	return inner, nil
}
