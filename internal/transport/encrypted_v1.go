package transport

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"

	"golang.org/x/crypto/scrypt"
)

// PSKTransport wraps another Transport with end-to-end AES-256-GCM
// authenticated encryption alone, so the transport's own provider only ever
// observes ciphertext. This is the PSK-only mode: no handshake, no key
// agreement, no rekeying — the v1 encrypted transport wire format (the
// EncryptedTransport of transport/encrypted.go at official repo, before
// the Noise v2 transport took over that file), where both peers derive
// directional keys from a shared secret.
//
// Keys are derived from the secret via scrypt; the context string (the
// document URL or transport type, public) is just a per-session KDF salt —
// secrecy comes exclusively from the secret, which both peers must share
// out of band. Each direction (client->exit, exit->client) uses its own
// derived key, so a compromise of one direction's traffic does not help
// decrypt the other. Every packet also carries a random nonce and is
// checked against a bounded replay window, so a captured packet cannot be
// replayed back at either peer.
//
// Frames on the wire:
//
//	OFX 0x01 <dir> | nonce (12 B) | ciphertext + tag
//
// dir is 0 for client->exit and 1 for exit->client; the 5-byte header is
// the AEAD associated data. A frame of any other shape, including the v2
// Noise format, is dropped.
type PSKTransport struct {
	Transport
	sendAEAD      cipher.AEAD
	receiveAEAD   cipher.AEAD
	sendDirection byte
	recvDirection byte

	seenMu    sync.Mutex
	seen      map[string]struct{}
	seenOrder []string
}

const (
	pskVersion       = byte(1)
	pskHeaderSize    = 5
	pskMaxSeenNonces = 4096
	pskMinSecretLen  = 16
)

// NewPSKTransport wraps inner with a directional AES-256-GCM stream. Both
// peers must be configured with the same secret and context, and exactly
// one of them must be the initiator (the client), so the two sides pick
// opposite send/receive key pairs.
func NewPSKTransport(inner Transport, secret, context string, initiator bool) (*PSKTransport, error) {
	if inner == nil {
		return nil, errors.New("inner transport is nil")
	}
	if len(secret) < pskMinSecretLen {
		return nil, fmt.Errorf("encryption secret must contain at least %d characters", pskMinSecretLen)
	}

	salt := sha256.Sum256([]byte("OpenFlux encrypted transport v1\x00" + context))
	master, err := scrypt.Key([]byte(secret), salt[:], 32768, 8, 1, 32)
	if err != nil {
		return nil, fmt.Errorf("derive encryption key: %w", err)
	}
	clientToExit := pskDirectionKey(master, "client-to-exit")
	exitToClient := pskDirectionKey(master, "exit-to-client")

	sendKey, receiveKey := clientToExit, exitToClient
	sendDirection, recvDirection := byte(0), byte(1)
	if !initiator {
		sendKey, receiveKey = exitToClient, clientToExit
		sendDirection, recvDirection = 1, 0
	}
	sendAEAD, err := newPSKGCM(sendKey)
	if err != nil {
		return nil, err
	}
	receiveAEAD, err := newPSKGCM(receiveKey)
	if err != nil {
		return nil, err
	}
	return &PSKTransport{
		Transport:     inner,
		sendAEAD:      sendAEAD,
		receiveAEAD:   receiveAEAD,
		sendDirection: sendDirection,
		recvDirection: recvDirection,
		seen:          make(map[string]struct{}),
	}, nil
}

// pskDirectionKey derives one direction's AES-256 key from the master key
// and the direction label, so the two directions never share a key.
func pskDirectionKey(master []byte, label string) []byte {
	mac := hmac.New(sha256.New, master)
	_, _ = mac.Write([]byte("OpenFlux direction v1\x00" + label))
	return mac.Sum(nil)
}

func newPSKGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create AES-GCM: %w", err)
	}
	return aead, nil
}

func (p *PSKTransport) Send(data []byte) error {
	header := []byte{encryptedMagic[0], encryptedMagic[1], encryptedMagic[2], pskVersion, p.sendDirection}
	nonce := make([]byte, p.sendAEAD.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("create packet nonce: %w", err)
	}
	packet := make([]byte, 0, len(header)+len(nonce)+len(data)+p.sendAEAD.Overhead())
	packet = append(packet, header...)
	packet = append(packet, nonce...)
	packet = p.sendAEAD.Seal(packet, nonce, data, header)
	return p.Transport.Send(packet)
}

func (p *PSKTransport) Receive(callback func([]byte)) {
	p.Transport.Receive(func(packet []byte) {
		if len(packet) < pskHeaderSize+p.receiveAEAD.NonceSize()+p.receiveAEAD.Overhead() {
			return
		}
		header := packet[:pskHeaderSize]
		if header[0] != encryptedMagic[0] || header[1] != encryptedMagic[1] ||
			header[2] != encryptedMagic[2] || header[3] != pskVersion ||
			header[4] != p.recvDirection {
			return
		}
		nonceEnd := pskHeaderSize + p.receiveAEAD.NonceSize()
		nonce := packet[pskHeaderSize:nonceEnd]
		plaintext, err := p.receiveAEAD.Open(nil, nonce, packet[nonceEnd:], header)
		if err != nil || !p.rememberNonce(nonce) {
			return
		}
		callback(plaintext)
	})
}

// rememberNonce reports whether nonce is fresh and, if so, records it in
// the bounded window of recently seen nonces.
func (p *PSKTransport) rememberNonce(nonce []byte) bool {
	key := string(nonce)
	p.seenMu.Lock()
	defer p.seenMu.Unlock()
	if _, exists := p.seen[key]; exists {
		return false
	}
	p.seen[key] = struct{}{}
	p.seenOrder = append(p.seenOrder, key)
	if len(p.seenOrder) > pskMaxSeenNonces {
		oldest := p.seenOrder[0]
		p.seenOrder = p.seenOrder[1:]
		delete(p.seen, oldest)
	}
	return true
}
