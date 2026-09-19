package encryptionsetup

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"openflux/internal/transport"
)

// Options are the encryption settings as given on the command
// line or by the mobile bridge. All empty means plaintext: encryption is
// opt-in, turned on by --exit-key-file on the exit node and --peer-key on
// the client.
type Options struct {
	ExitKeyFile string // exit node: its static key file, created on first use
	PeerKey     string // client: the exit node's public key (base64)
	PSK         string // both, optional: the shared secret that closes the node to strangers
}

// Setup is a configured encryption layer ready to wrap raw transports (one
// per document in a multi-stream tunnel). Exactly one of Wrap/WrapOverCodec
// is set: Wrap sits directly on the raw backend, under the codec (the Noise
// v2 layering, wire = noise(batch)); WrapOverCodec wraps the codec instead
// (the v1 layering, wire = batch(v1 frame), spoken by old
// --encryption-key-file clients, so the PSK-only mode stays cross-version
// compatible). The context string (the document URL or transport type)
// salts the PSK-only KDF.
type Setup struct {
	Wrap          func(transport.Transport) (transport.Transport, error)
	WrapOverCodec func(transport.Transport, string) (transport.Transport, error)
	Label         string // one line for the startup log
	Banner        string // exit node only: the public key to hand to clients
}

// New validates the options for this side and prepares the
// layer. It returns nil, nil when no encryption option was given: the tunnel
// then runs plaintext (encryption is opt-in). A PSK without static keys
// selects the PSK-only mode: the v1 AES-256-GCM transport with no handshake,
// so neither side needs a key file.
func New(opts Options, initiator bool) (*Setup, error) {
	if opts.ExitKeyFile == "" && opts.PeerKey == "" && opts.PSK == "" {
		return nil, nil
	}
	if opts.PSK != "" && len(opts.PSK) < 16 {
		return nil, fmt.Errorf("--psk-file: encryption secret must contain at least 16 characters")
	}
	if opts.ExitKeyFile == "" && opts.PeerKey == "" {
		return &Setup{
			WrapOverCodec: func(inner transport.Transport, context string) (transport.Transport, error) {
				return transport.NewPSKTransport(inner, opts.PSK, context, initiator)
			},
			Label: "AES-256-GCM (PSK only, no handshake): both peers need the same --psk-file",
		}, nil
	}
	var psk []byte
	if opts.PSK != "" {
		var err error
		psk, err = transport.DerivePSK(opts.PSK)
		if err != nil {
			return nil, fmt.Errorf("--psk-file: %w", err)
		}
	}
	cfg := transport.EncryptedConfig{Initiator: initiator, PSK: psk}
	var banner string
	if initiator {
		if opts.ExitKeyFile != "" {
			return nil, errors.New("--exit-key-file belongs on the exit node; the client takes --peer-key")
		}
		if opts.PeerKey == "" {
			return nil, errors.New("encryption on the client needs --peer-key=<exit public key>; --psk-file alone only adds client authorization")
		}
		pub, err := transport.ParsePublicKey(opts.PeerKey)
		if err != nil {
			return nil, fmt.Errorf("invalid peer key: %w", err)
		}
		cfg.PeerStatic = pub
	} else {
		if opts.PeerKey != "" {
			return nil, errors.New("--peer-key belongs on the client; the exit node takes --exit-key-file")
		}
		if opts.ExitKeyFile == "" {
			return nil, errors.New("encryption on the exit node needs --exit-key-file=<path>; --psk-file alone only adds client authorization")
		}
		key, created, err := transport.LoadOrCreateStaticKey(opts.ExitKeyFile)
		if err != nil {
			return nil, err
		}
		cfg.StaticKey = key
		pub := transport.PublicKeyString(key.Public)
		state := "loaded from"
		if created {
			state = "generated and saved to"
		}
		banner = fmt.Sprintf("\n=== EXIT PUBLIC KEY (%s %s) ===\n%s\nStart clients with --peer-key=%s\n\n",
			state, opts.ExitKeyFile, pub, pub)
	}

	label := "Noise NKpsk0 (X25519 + AES-256-GCM), open node: any client with the public key may connect"
	if psk != nil {
		label = "Noise NKpsk0 (X25519 + AES-256-GCM), closed node: PSK required"
	}

	return &Setup{
		Wrap: func(inner transport.Transport) (transport.Transport, error) {
			return transport.NewEncryptedTransport(inner, cfg)
		},
		Label:  label,
		Banner: banner,
	}, nil
}

// ReadSecretFile reads a secret from path, ignoring surrounding whitespace
// (editors love trailing newlines).
func ReadSecretFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read secret file: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}
