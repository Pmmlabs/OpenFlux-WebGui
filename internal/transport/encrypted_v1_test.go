package transport

import (
	"testing"
)

const pskTestSecret = "a sufficiently long shared secret"
const pskTestContext = "https://disk.yandex.com/i/test-doc"

// newPSKPair builds a PSK-only client and exit joined by two manual wires,
// mirroring encrypted_test.go's pair but without any handshake. Receive
// must be called on both ends before delivery: the decrypting handler is
// registered on the raw wire then.
func newPSKPair(t *testing.T, clientSecret, exitSecret, clientContext, exitContext string) (client, exit *PSKTransport, clientWire, exitWire *wireEnd) {
	t.Helper()
	clientWire = &wireEnd{}
	exitWire = &wireEnd{}
	client, err := NewPSKTransport(clientWire, clientSecret, clientContext, true)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	exit, err = NewPSKTransport(exitWire, exitSecret, exitContext, false)
	if err != nil {
		t.Fatalf("exit: %v", err)
	}
	return client, exit, clientWire, exitWire
}

func TestPSKTransportRoundtrip(t *testing.T) {
	client, exit, clientWire, exitWire := newPSKPair(t, pskTestSecret, pskTestSecret, pskTestContext, pskTestContext)

	var got [][]byte
	exit.Receive(func(data []byte) { got = append(got, append([]byte(nil), data...)) })
	var clientGot [][]byte
	client.Receive(func(data []byte) { clientGot = append(clientGot, append([]byte(nil), data...)) })

	for _, payload := range [][]byte{[]byte("hello"), []byte(""), make([]byte, 4096)} {
		if err := client.Send(payload); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	for _, frame := range clientWire.take() {
		exitWire.deliver(frame)
	}
	if len(got) != 3 || string(got[0]) != "hello" || len(got[1]) != 0 || len(got[2]) != 4096 {
		t.Fatalf("exit got %d packets: %q, %d, %d", len(got), got[0], len(got[1]), len(got[2]))
	}

	// The exit answers on its own direction key.
	if err := exit.Send([]byte("pong")); err != nil {
		t.Fatalf("exit send: %v", err)
	}
	for _, frame := range exitWire.take() {
		clientWire.deliver(frame)
	}
	if len(clientGot) != 1 || string(clientGot[0]) != "pong" {
		t.Fatalf("client got %q", clientGot)
	}
}

func TestPSKTransportRejectsWrongSecret(t *testing.T) {
	client, exit, clientWire, exitWire := newPSKPair(t,
		"a different sufficiently long secret", pskTestSecret, pskTestContext, pskTestContext)

	var got int
	exit.Receive(func([]byte) { got++ })
	if err := client.Send([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	for _, frame := range clientWire.take() {
		exitWire.deliver(frame)
	}
	if got != 0 {
		t.Fatal("exit accepted a frame encrypted with the wrong secret")
	}
}

func TestPSKTransportRejectsContextMismatch(t *testing.T) {
	client, exit, clientWire, exitWire := newPSKPair(t,
		pskTestSecret, pskTestSecret, "https://disk.yandex.com/i/other-doc", pskTestContext)

	var got int
	exit.Receive(func([]byte) { got++ })
	if err := client.Send([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	for _, frame := range clientWire.take() {
		exitWire.deliver(frame)
	}
	if got != 0 {
		t.Fatal("exit accepted a frame derived with a different context salt")
	}
}

func TestPSKTransportRejectsTampering(t *testing.T) {
	client, exit, clientWire, exitWire := newPSKPair(t, pskTestSecret, pskTestSecret, pskTestContext, pskTestContext)

	var got int
	exit.Receive(func([]byte) { got++ })

	// A v2 Noise frame must be dropped too: different version byte.
	noiseFrame := make([]byte, 32)
	noiseFrame[0], noiseFrame[1], noiseFrame[2] = encryptedMagic[0], encryptedMagic[1], encryptedMagic[2]
	noiseFrame[3], noiseFrame[4] = encryptedVersion, msgHandshakeInit
	exitWire.deliver(noiseFrame)

	// A genuine frame with a flipped ciphertext bit must fail authentication.
	if err := client.Send([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	frames := clientWire.take()
	if len(frames) != 1 {
		t.Fatalf("client sent %d frames", len(frames))
	}
	tampered := append([]byte(nil), frames[0]...)
	tampered[len(tampered)-1] ^= 0xff
	exitWire.deliver(tampered)

	// The untouched original still decrypts afterwards.
	exitWire.deliver(frames[0])
	if got != 1 {
		t.Fatalf("got %d packets, want 1 (tampering or v2 frame accepted?)", got)
	}
}

func TestPSKTransportRejectsReplay(t *testing.T) {
	client, exit, clientWire, exitWire := newPSKPair(t, pskTestSecret, pskTestSecret, pskTestContext, pskTestContext)

	var got int
	exit.Receive(func([]byte) { got++ })
	if err := client.Send([]byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := client.Send([]byte("second")); err != nil {
		t.Fatal(err)
	}
	frames := clientWire.take()
	for _, frame := range frames {
		exitWire.deliver(frame)
	}
	if got != 2 {
		t.Fatalf("got %d packets, want 2", got)
	}
	// Replay the first frame: authentic but its nonce was already seen.
	exitWire.deliver(frames[0])
	if got != 2 {
		t.Fatalf("replayed frame accepted: got %d packets", got)
	}
}

func TestPSKTransportRejectsWrongDirection(t *testing.T) {
	client, _, clientWire, _ := newPSKPair(t, pskTestSecret, pskTestSecret, pskTestContext, pskTestContext)

	var got int
	client.Receive(func([]byte) { got++ })
	if err := client.Send([]byte("loopback")); err != nil {
		t.Fatal(err)
	}
	// Feed the client's own outgoing frame back to it: direction byte 0 is
	// only accepted from the exit side, so it must be dropped.
	for _, frame := range clientWire.take() {
		clientWire.deliver(frame)
	}
	if got != 0 {
		t.Fatal("client accepted its own outgoing frame")
	}
}

func TestPSKTransportRejectsShortSecret(t *testing.T) {
	if _, err := NewPSKTransport(&wireEnd{}, "short", pskTestContext, true); err == nil {
		t.Fatal("short secret accepted")
	}
}
