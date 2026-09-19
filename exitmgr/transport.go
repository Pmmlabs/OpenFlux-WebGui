package exitmgr

import (
	"fmt"
	"os"
	"strings"

	"github.com/flynn/noise"

	"openflux/transport"
	"openflux/transportstack"
)

// BuildTransport constructs the full transport stack for one client,
// delegating the actual backend/encryption/codec/multi-stream wiring to
// transportstack.Build -- the same code path main.go's CLI dispatch uses,
// so a client added through the panel is wired identically to one started
// via CLI flags and the two can't silently diverge on layering order again
// (see transportstack's package doc for what happened when they did).
// Every client shares the panel's own static key (staticKey, loaded once
// by runExitPanel); cfg.PSKFile optionally closes this one client to
// strangers who don't have the shared secret. A zero staticKey (no
// --panel-key-file) leaves the tunnel plaintext -- or PSK-only when the
// client has a psk_file: the v1 AES-256-GCM transport layered over the
// codec (wire = batch(v1 frame)), so old --encryption-key-file clients
// interoperate, matching the CLI's opt-in encryption.
func BuildTransport(cfg ClientConfig, base transport.TransportConfig, staticKey noise.DHKey) (transport.Transport, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	var pskSecret string
	if cfg.PSKFile != "" {
		secretBytes, err := os.ReadFile(cfg.PSKFile)
		if err != nil {
			return nil, fmt.Errorf("read psk file: %w", err)
		}
		pskSecret = strings.TrimSpace(string(secretBytes))
	}

	// With a panel key (--panel-key-file) every client tunnel is Noise
	// NKpsk0 under the codec; a psk_file then additionally closes the node
	// to strangers. Without a panel key a psk_file alone selects the
	// PSK-only v1 transport over the codec instead.
	var encryptFn func(transport.Transport) (transport.Transport, error)
	var encryptOverCodecFn func(transport.Transport, string) (transport.Transport, error)
	if staticKey.Private != nil {
		var psk []byte
		if pskSecret != "" {
			var err error
			psk, err = transport.DerivePSK(pskSecret)
			if err != nil {
				return nil, fmt.Errorf("psk file: %w", err)
			}
		}
		encryptFn = func(inner transport.Transport) (transport.Transport, error) {
			return transport.NewEncryptedTransport(inner, transport.EncryptedConfig{
				Initiator: false,
				StaticKey: staticKey,
				PSK:       psk,
			})
		}
	} else if pskSecret != "" {
		encryptOverCodecFn = func(inner transport.Transport, context string) (transport.Transport, error) {
			return transport.NewPSKTransport(inner, pskSecret, context, false)
		}
	}

	return transportstack.Build(transportstack.Params{
		TransportType:    cfg.Transport,
		URL:              cfg.URL,
		IsExit:           true, // panel clients are always the exit side
		MaxToken:         cfg.MaxToken,
		MaxUid:           cfg.MaxUid,
		Codec:            cfg.Codec,
		Encrypt:          encryptFn,
		EncryptOverCodec: encryptOverCodecFn,
	}, base)
}
