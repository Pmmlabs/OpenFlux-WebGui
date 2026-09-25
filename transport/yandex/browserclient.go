package yandex

import (
	"context"
	"net"
	"net/http"
	"time"

	utls "github.com/refraction-networking/utls"
)

// browserUserAgent — UA, согласованный с browserClientHello и
// greedFingerprintTemplate (Chrome на macOS).
const browserUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.7390.66 Safari/537.36"

// browserClientHello — имитация ClientHello настоящего Chrome.
// Стандартный TLS-клиент Go — известный бот-сигнал для Яндекс-антибота:
// с ним после showcaptchafast всегда приходит вторая, «checkbox»-капча.
var browserClientHello = utls.HelloChrome_133

// newBrowserHTTPClient — http.Client с браузерным TLS-фингерпринтом.
// Яндекс различает Go-клиент и браузер по ClientHello, поэтому все запросы
// к docs/disk.yandex.ru (документ, капчи) должны идти через этот клиент.
func newBrowserHTTPClient(jar http.CookieJar, timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				dialer := &net.Dialer{Timeout: 10 * time.Second}
				rawConn, err := dialer.DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				conn := utls.UClient(rawConn, &utls.Config{ServerName: host}, browserClientHello)
				if err := conn.HandshakeContext(ctx); err != nil {
					rawConn.Close()
					return nil, err
				}
				return conn, nil
			},
			DisableKeepAlives: true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: timeout,
		Jar:     jar,
	}
}
