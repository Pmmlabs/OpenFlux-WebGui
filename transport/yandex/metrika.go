package yandex

import (
	"context"
	"crypto/tls"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"

	"openflux/utils"
)

// Эмуляция маяков Яндекс.Метрики, которые настоящий браузер отправляет
// при рендере страницы капчи. Сеть захвачена с Chrome 141 (macOS):
// бэкенд капчи сверяет watch-хит счётчика 105560523, помеченный
// site-info={"unique_key":"<uniqueKey капчи>"}, с сессией капчи —
// без него сабмит выглядит ботным и эскалируется до силуэтной капчи.

// metrikaUAH — UA-CH данные (параметр uah watch-хита), статичны для
// Chrome 141 на macOS.
const metrikaUAH = "chu\n" +
	"\"Google Chrome\";v=\"141\",\"Not?A_Brand\";v=\"8\",\"Chromium\";v=\"141\"\n" +
	"cha\narm\nchb\n64\nchf\n141.0.7390.66\n" +
	"chl\n" +
	"\"Google Chrome\";v=\"141.0.7390.66\",\"Not?A_Brand\";v=\"8.0.0.0\",\"Chromium\";v=\"141.0.7390.66\"\n" +
	"chm\n?0\nchp\nmacOS\nchv\n14.7.4"

const (
	metrikaTagURL     = "https://mc.yandex.ru/metrika/tag.js?id=105560523"
	metrikaCounterURL = "https://mc.yandex.ru/watch/105560523"
	metrikaWatch3URL  = "https://mc.yandex.ru/watch/3/1"
	metrikaRTCounter  = 99742118
	// Параметры «браузера» из трейса headless Chrome 1280x800.
	metrikaWindow = "1280x713"
	metrikaScreen = "800x600x24"
	metrikaDS     = "0,0,23,31,651,0,,2,0,1518,,,806"
)

var (
	metrikaStateOnce sync.Once
	metrikaVf        string
	metrikaHid       int64
	metrikaLs        int64
)

// metrikaBrowserState — стабильные на протяжении «жизни браузера» значения
// из browser-info (vf — отпечаток, hid — id сессии, ls — время записи в
// localStorage). Генерируются один раз на процесс.
func metrikaBrowserState() (vf string, hid int64, ls int64) {
	metrikaStateOnce.Do(func() {
		const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
		b := make([]byte, 31)
		for i := range b {
			b[i] = alphabet[rand.Intn(len(alphabet))]
		}
		metrikaVf = string(b)
		metrikaHid = rand.Int63n(900000000) + 100000000
		// localStorage пишется при первом визите — давно.
		metrikaLs = time.Now().Add(-time.Duration(rand.Intn(3000)+300) * 24 * time.Hour).UnixMilli()
	})
	return metrikaVf, metrikaHid, metrikaLs
}

func metrikaGet(client *http.Client, rawURL, referer, userAgent string) {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Referer", referer)
	req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7")
	resp, err := client.Do(req)
	if err != nil {
		utils.Debugf("[METRIKA] GET failed: %v", err)
		return
	}
	if resp.Body != nil {
		drain := make([]byte, 8192)
		for {
			if _, err := resp.Body.Read(drain); err != nil {
				break
			}
		}
		resp.Body.Close()
	}
}

// metrikaQuery — сборка query в порядке следования параметров (как в
// запросах самого tag.js), url.Values сортирует ключи.
func metrikaQuery(params [][2]string) string {
	parts := make([]string, 0, len(params))
	for _, p := range params {
		parts = append(parts, url.QueryEscape(p[0])+"="+url.QueryEscape(p[1]))
	}
	return strings.Join(parts, "&")
}

// sendMetrikaBeacons — маяки страницы капчи: tag.js, watch-пиксель
// счётчика 3, watch-хит счётчика капчи 105560523 с unique_key и rt-хит.
// mc.yandex.ru работает по HTTP/2 (браузер всегда ходит туда по h2),
// поэтому транспорт — http2 поверх utls.
func sendMetrikaBeacons(jar http.CookieJar, pageURL, uniqueKey, pageTitle, userAgent string) {
	t2 := &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			rawConn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
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
	}
	client := &http.Client{
		Transport: t2,
		Jar:       jar,
		Timeout:   10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	vf, hid, ls := metrikaBrowserState()
	now := time.Now()
	uts := now.UnixMicro()
	// yu — uid Метрики: <9 случайных цифр><unix seconds>.
	yu := fmt.Sprintf("%09d%d", rand.Intn(900000000)+100000000, now.Unix())
	rn := rand.Intn(900000000) + 100000000

	// 1. tag.js (загружается страницей капчи).
	metrikaGet(client, metrikaTagURL, pageURL, userAgent)

	// 2. watch-пиксель служебного счётчика 3.
	q3 := metrikaQuery([][2]string{
		{"wmode", "7"},
		{"page-url", pageURL},
		{"page-ref", ""},
		{"charset", "utf-8"},
		{"ut", "noindex"},
		{"browser-info", fmt.Sprintf("pv:1:vf:%s:fu:0:en:utf-8:la:ru-RU:v:2660:cn:1:dp:0:ls:%d:hid:%d:z:180:i:%s:et:%d:c:1:rn:%d:rqn:1:u:%d:w:%s:s:%s:sk:1:fp:1448:wv:2:ds:%s:co:0:hdl:1:cpf:1:ns:%d:st:%d",
			vf, ls, hid, now.Format("20060102150405"), now.Unix(), rn, uts, metrikaWindow, metrikaScreen, metrikaDS, now.UnixMilli(), now.UnixMilli())},
		{"t", fmt.Sprintf("clc(0-0-0)rqnt(1)aw(1)rcm(1)yu(%s)cdl(na)eco(131072)ti(1)", yu)},
	})
	metrikaGet(client, metrikaWatch3URL+"?"+q3, pageURL, userAgent)

	// 3. watch-хит счётчика капчи 105560523 с unique_key — ключевой хит,
	// по нему бэкенд связывает маяк с сессией капчи.
	q5 := metrikaQuery([][2]string{
		{"wmode", "7"},
		{"page-url", pageURL},
		{"charset", "utf-8"},
		{"site-info", fmt.Sprintf(`{"unique_key":"%s"}`, uniqueKey)},
		{"ut", "noindex"},
		{"uah", metrikaUAH},
		{"browser-info", fmt.Sprintf("pv:1:vf:%s:fu:0:en:utf-8:la:ru-RU:v:2660:cn:2:dp:0:ls:%d:hid:%d:z:180:i:%s:et:%d:c:1:rn:%d:rqn:1:u:%d:w:%s:s:%s:sk:1:fp:1448:wv:2:ds:%s:co:0:hdl:1:cpf:1:ns:%d:adb:2:rqnl:1:st:%d:t:%s",
			vf, ls, hid, now.Format("20060102150405"), now.Unix(), rn, uts, metrikaWindow, metrikaScreen, metrikaDS, now.UnixMilli(), now.UnixMilli(), url.QueryEscape(pageTitle))},
		{"t", fmt.Sprintf("clt(%d)gdpr(8-0)clc(0-0-0)rqnt(1)aw(1)rcm(1)yu(%s)ecs(0)cdl(na)eco(84558852)ti(1)", rand.Intn(900)+100, yu)},
	})
	metrikaGet(client, metrikaCounterURL+"?"+q5, pageURL, userAgent)

	// 4. rt-хит дочернего счётчика.
	rtq := metrikaQuery([][2]string{
		{"browser-info", fmt.Sprintf("rt:1:u:%d", uts)},
		{"page-url", pageURL},
		{"page-ref", ""},
		{"site-info", `{"counterId":105560523,"cnt-class":"0"}`},
	})
	rtHost := fmt.Sprintf("%010d.mc.yandex.ru", rand.Int63n(9000000000)+1000000000)
	metrikaGet(client, fmt.Sprintf("https://%s/watch/%d/1?%s", rtHost, metrikaRTCounter, rtq), pageURL, userAgent)

	// 5. Сопутствующие подресурсы tag.js.
	metrikaGet(client, "https://mc.yandex.ru/metrika/match.html", pageURL, userAgent)
	metrikaGet(client, "https://mc.yandex.ru/metrika/advert.gif", pageURL, userAgent)
	metrikaGet(client, "https://mc.yandex.ru/metrika/tag_phono.js", pageURL, userAgent)

	// 6. adfstat-пиксель (host только http/1.1 — отдельный клиент).
	adfstatClient := newBrowserHTTPClient(nil, 10*time.Second)
	metrikaGet(adfstatClient, "https://adfstat.yandex.ru/metrica?id=354083781", pageURL, userAgent)

	utils.Debugf("[METRIKA] beacons sent for %s unique_key=%s", shortStr(pageURL, 80), shortStr(uniqueKey, 20))
}
