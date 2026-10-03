package yandex

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"openflux/transport"
	"openflux/utils"
)

// Precompiled once. cursorPayloadRe in particular runs on every inbound
// message, so compiling it per call (as before) was pure overhead on the hot
// receive path.
var (
	cursorPayloadRe = regexp.MustCompile(`"cursor":"[^;]+;([^"]+)"`)
	messageTypeRe   = regexp.MustCompile(`"type":"([^"]+)"`)
	sidRe           = regexp.MustCompile(`"sid":"([^"]+)"`)
	authResultRe    = regexp.MustCompile(`"type":"auth","result":(\d+),"sessionId":"([^"]+)"`)
	clientConfigRe  = regexp.MustCompile(`<script[^>]*id="client-config"[^>]*>(.*?)</script>`)
)

// Сколько ждать подтверждения нашей editor-сессии (auth result:1 с нашим
// sessionId) до принудительного реконнекта. Здоровое подтверждение
// приходит за ~5с; 60с (серверный drop ждущего) - слишком долго для
// молча мёртвого туннеля.
// authWatchdogInterval должен быть БОЛЬШЕ серверного таймера лока
// (services.CoAuthoring.expire.lockDoc = 30с, см. DocsCoServer.js
// setLockDocumentTimer): при молчащем/мёртвом держателе сервер сам
// форс-анлокает лок и authed ждущего по таймеру. Если наш watchdog
// сработает раньше и переподключится, каждый новый джойн сбрасывает
// серверный таймер (cleanLockDocumentTimer+setLockDocumentTimer при
// re-lock) - и цикл waitAuth становится вечным.
const authWatchdogInterval = 45 * time.Second

// Прогрессивная пауза при силуэтной эскалации капчи (бот-флаг IP):
// 30м -> 1ч -> 2ч -> 4ч (потолок). См. YandexDocsTransport.botFlagCooldownN.
const (
	botFlagCooldownBase = 30 * time.Minute
	botFlagCooldownMax  = 4 * time.Hour
)

type YandexDocsInfo struct {
	CookieStr   string
	Token       string
	DocID       string
	CallbackURL string
	UserID      string
	Origin      string
	Host        string
	WsURL       string
	Permissions map[string]interface{}
	OpenCmd     map[string]interface{}
}

type DocSession struct {
	Info       YandexDocsInfo
	Conn       *websocket.Conn
	WriteQueue chan []byte
	UserID     string
	writeMu    sync.Mutex

	// Наш socket.io sid (из handshake-кадра "0{\"sid\":...}") и признак
	// подтверждения нашей editor-сессии сервером (auth result:1 с нашим
	// sessionId). До подтверждения сервер не релеит наши сообщения
	// участникам - туннель молча мёртв, поэтому connectToDoc держит
	// auth-watchdog.
	sid        string
	authMu     sync.Mutex
	authed     bool
	authFailed bool
}

// AuthConfirmed reports whether the server accepted our editor session.
func (s *DocSession) AuthConfirmed() bool {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	return s.authed
}

// markAuth records the server's verdict for our own sessionId.
func (s *DocSession) markAuth(ok bool) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if ok {
		s.authed = true
	} else {
		s.authFailed = true
	}
}

// AuthRejected reports whether the server explicitly rejected our session.
func (s *DocSession) AuthRejected() bool {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	return s.authFailed
}

func (s *DocSession) safeWrite(messageType int, data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.Conn.WriteMessage(messageType, data)
}

type YandexDocsTransport struct {
	*transport.BaseTransport

	url     string
	session *DocSession
	// jar is shared across reconnects so a captcha-passed session is
	// reused instead of re-solving the captcha on every connect (each
	// solve burns IP reputation; ~40 solves trigger the unsolvable
	// silhouette escalation).
	jar *persistJar

	userCounter atomic.Int32
	baseUserID  string
	// botFlagCooldownN - сколько раз подряд fetchDocInfo упирался в
	// силуэтную эскалацию. Прогрессивная пауза между попытками
	// (30м -> 1ч -> 2ч -> 4ч, потолок 4ч): флаг бота распадается медленно,
	// а каждая попытка - это капча + GREED VM (на слабом ARM роутера -
	// десятки секунд 100% CPU) и минус репутация. Плоские 30 минут дают
	// вечный CPU-цикл каждые полчаса. Сбрасывается при успешном fetchDocInfo.
	botFlagCooldownN atomic.Int32
}

func NewYandexDocsTransport(url string, config transport.TransportConfig) *YandexDocsTransport {
	t := &YandexDocsTransport{
		BaseTransport: transport.NewBaseTransport(config),
		url:           url,
		jar:           newPersistJar(config.CookieFile),
	}
	t.baseUserID = randUserID()
	return t
}

func (t *YandexDocsTransport) Start() error {
	if err := t.BaseTransport.Start(); err != nil {
		return err
	}

	t.baseUserID = randUserID()
	utils.SafeGo("yandex.keepAlive", t.keepAliveLoop)
	t.connectToDoc(0)

	return nil
}

func (t *YandexDocsTransport) Send(data []byte) error {
	if !t.IsConnected() {
		return fmt.Errorf("transport not connected")
	}

	t.Mu.RLock()
	session := t.session
	t.Mu.RUnlock()

	if session == nil {
		return fmt.Errorf("no active session")
	}

	select {
	case session.WriteQueue <- data:
		t.RecordSend(len(data))
		return nil
	default:
		return fmt.Errorf("write queue full")
	}
}

func (t *YandexDocsTransport) connectToDoc(attempt int) {
	if !t.IsRunning() {
		return
	}

	utils.Debugf("[YDOCS] connectToDoc attempt ...")

	go func() {
		defer func() {
			if r := recover(); r != nil {
				utils.Debugf("[PANIC] recovered in yandex.connect: %v", r)
			}
		}()
		// Свежий userID на каждую попытку. Переиспользование ID только
		// что убитого участника сервер отвергает мгновенным close 1005
		// (duplicate participant), превращая реконнект в цикл
		// connect->kick->connect: пока призрак прошлого подключения не
		// истечёт по TTL, зайти под тем же ID невозможно.
		suffix := fmt.Sprintf("%03d", t.userCounter.Add(1)%1000)
		userID := t.baseUserID + suffix

		info, err := t.fetchDocInfo(t.url, userID)
		if err != nil {
			utils.Debugf("[YDOCS] fetchDocInfo failed: %v", err)
			// Серьёзная эскалация капчи: IP помечен ботным, флаг распадается
			// медленно. Прогрессивная пауза (30м -> 1ч -> 2ч -> 4ч): каждая
			// попытка жжёт репутацию и греет CPU (капча + GREED VM), поэтому
			// чем дольше флаг держится, тем реже пробуем.
			if strings.Contains(err.Error(), "escalated to silhouette") {
				n := t.botFlagCooldownN.Add(1)
				d := botFlagCooldownBase << (n - 1)
				if d > botFlagCooldownMax || d <= 0 { // guard от переполнения сдвига
					d = botFlagCooldownMax
				}
				utils.Debugf("[YDOCS] bot-flagged: cooling down %v before next attempt (streak %d)", d, n)
				time.Sleep(d)
				if !t.IsRunning() {
					return
				}
			}
			t.scheduleReconnect(attempt)
			return
		}
		// Капча пройдена (или не требовалась) - бот-флаг спал, сбрасываем
		// прогрессию cooldown'а.
		t.botFlagCooldownN.Store(0)

		// Hard TCP dial timeout so a stuck connect/DNS to the balancer host
		// can't hang the whole transport (HandshakeTimeout alone proved
		// insufficient on iOS).
		dialer := websocket.Dialer{
			HandshakeTimeout: 15 * time.Second,
			NetDialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		}
		headers := http.Header{}
		headers.Set("User-Agent", "Mozilla/5.0")
		headers.Set("Origin", info.Origin)
		headers.Set("Cookie", info.CookieStr)
		headers.Set("Host", info.Host)

		utils.Debugf("[YDOCS] WebSocket dial %s", info.WsURL)
		conn, resp, err := dialer.Dial(info.WsURL, headers)
		if err != nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			utils.Debugf("[YDOCS] WebSocket dial failed (http %d): %v", status, err)
			t.scheduleReconnect(attempt)
			return
		}
		utils.Debugf("[YDOCS] WebSocket connected to %s", info.Host)

		writeQueue := make(chan []byte, t.GetConfig().MaxQueueSize)
		t.Mu.Lock()
		if t.session != nil {
			writeQueue = t.session.WriteQueue
		}
		t.Mu.Unlock()

		session := &DocSession{
			Info:       info,
			Conn:       conn,
			WriteQueue: writeQueue,
			UserID:     userID,
		}

		t.Mu.Lock()
		hadSession := t.session != nil
		t.session = session
		t.SetConnected(true)
		t.Mu.Unlock()

		if !hadSession {
			utils.SafeGo("yandex.writer", t.writerLoop)
		}

		// Auth - use safeWrite
		auth1 := fmt.Sprintf(`40{"token":"%s"}`, info.Token)
		session.safeWrite(websocket.TextMessage, []byte(auth1))

		authData := map[string]interface{}{
			"type": "auth", "docid": info.DocID, "token": "fghhfgsjdgfjs",
			"user": map[string]interface{}{"id": userID}, "editorType": 0,
			"lastOtherSaveTime": -1, "permissions": info.Permissions,
			"openCmd": info.OpenCmd, "coEditingMode": "fast", "jwtOpen": info.Token,
		}
		messagePart, _ := json.Marshal([]interface{}{"message", authData})
		session.safeWrite(websocket.TextMessage, []byte(fmt.Sprintf("42%s", string(messagePart))))

		// Auth-watchdog: если сервер не подтверждает нашу сессию дольше
		// authWatchdogInterval - рвём соединение сами и переподключаемся
		// со свежим userID. Интервал заведомо больше серверного таймера
		// лока (30с): при молчащем держателе сервер сам форс-анлокает лок
		// и authed ждущего раньше, чем сработает watchdog.
		watchdog := time.AfterFunc(authWatchdogInterval, func() {
			if !session.AuthConfirmed() && session.sid != "" {
				utils.Debugf("[YDOCS] auth not confirmed in %v, reconnecting", authWatchdogInterval)
				conn.Close()
			}
		})
		defer watchdog.Stop()

		connectedAt := time.Now()
		for t.IsRunning() {
			_, message, err := conn.ReadMessage()
			if err != nil {
				utils.Debugf("[YDOCS] Read error: %v", err)
				t.SetConnected(false)
				conn.Close()
				// If the session was healthy for a while, treat the next
				// connect as fresh (attempt -1 -> next attempt 0) so backoff
				// doesn't keep growing across normal long-lived reconnects.
				next := attempt
				if time.Since(connectedAt) > 15*time.Second {
					next = -1
				}
				t.scheduleReconnect(next)
				return
			}
			t.handleMessage(session, message)
		}
	}()
}

func (t *YandexDocsTransport) writerLoop() {
	// The write queue is created once and preserved across reconnects, so we
	// capture it and block on it instead of polling with a 10ms sleep. The old
	// poll added up to 10ms of latency to every send and woke the CPU 100x/sec
	// while idle.
	var queue chan []byte
	for t.IsRunning() && queue == nil {
		t.Mu.Lock()
		if t.session != nil {
			queue = t.session.WriteQueue
		}
		t.Mu.Unlock()
		if queue == nil {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if queue == nil {
		return
	}

	var pending []byte
	// Сессия, запись в которую уже падала: после close-хендшейка (gorilla
	// отвечает close-кадром на close сервера) любые записи в неё навсегда
	// дают "websocket: close sent". Ретраи против мёртвой сессии каждые
	// 15мс только заливают лог (~150 строк "Write error" за реконнект) -
	// ждём подмены сессии реконнектом, ошибку логируем один раз.
	var failedSession *DocSession
	for t.IsRunning() {
		if pending == nil {
			packet, ok := <-queue
			if !ok {
				return
			}
			pending = packet
		}

		t.Mu.RLock()
		session := t.session
		t.Mu.RUnlock()
		if session == nil || session.Conn == nil {
			// Mid-reconnect: hold the packet and retry rather than drop it.
			time.Sleep(15 * time.Millisecond)
			continue
		}

		payload := base64.StdEncoding.EncodeToString(pending)
		msg := fmt.Sprintf(`42["message",{"type":"cursor","cursor":"18;%s"}]`, payload)
		if err := session.safeWrite(websocket.TextMessage, []byte(msg)); err != nil {
			if session != failedSession {
				utils.Debugf("[YDOCS] Write error: %v (holding packet until reconnect)", err)
				failedSession = session
			}
			time.Sleep(100 * time.Millisecond)
			continue // keep pending; the reconnect will bring up a new conn
		}
		failedSession = nil
		pending = nil
	}
}

func (t *YandexDocsTransport) keepAliveLoop() {
	ticker := time.NewTicker(t.GetConfig().KeepAliveInterval)
	defer ticker.Stop()
	keepAliveMsg := `42["message",{"type":"cursor","cursor":"18;---KA---"}]`

	for t.IsRunning() {
		<-ticker.C
		t.Mu.Lock()
		session := t.session
		t.Mu.Unlock()

		if session != nil && session.Conn != nil {
			if err := session.safeWrite(websocket.TextMessage, []byte(keepAliveMsg)); err != nil {
				utils.Debugf("[YDOCS] Keep-alive failed: %v", err)
				t.SetConnected(false)
			}
		}
	}
}

func (t *YandexDocsTransport) handleMessage(session *DocSession, data []byte) {
	text := string(data)

	// Socket.IO handshake: запоминаем свой sid - по нему опознаём свой
	// auth result среди broadcast-сообщений чужих подключений.
	if strings.HasPrefix(text, "0{") || strings.HasPrefix(text, "40{") {
		if m := sidRe.FindStringSubmatch(text); m != nil {
			session.sid = m[1]
			utils.Debugf("[YDOCS] socket.io sid=%s", session.sid)
		}
		return
	}

	// Подтверждение/отвержение нашей editor-сессии. auth-кадры
	// рассылаются всем участникам, поэтому сверяем sessionId со своим.
	if m := authResultRe.FindStringSubmatch(text); m != nil && m[2] == session.sid {
		if m[1] == "1" {
			session.markAuth(true)
			utils.Debugf("[YDOCS] auth confirmed (sid=%s)", session.sid)
		} else {
			session.markAuth(false)
			utils.Debugf("[YDOCS] auth rejected (result=%s), reconnecting", m[1])
			session.Conn.Close()
		}
		return
	}

	if strings.Contains(text, "---KA---") {
		return
	}

	// Idle-предупреждение сессии (code 4002, у Яндекса interval=20 мин):
	// сервер засчитывает активность ТОЛЬКО сообщением extendSession -
	// курсор-KA не считается (DocsCoServer.js обновляет
	// sessionTimeLastAction лишь в case 'extendSession'). Браузерный
	// редактор отвечает на предупреждение extendSession; без ответа
	// сервер рвёт сессию disconnectReason 4002 каждые ~20 мин, а каждый
	// такой разрыв - обрыв туннеля, реконнект-шторм клиентов и (на
	// слабом CPU) всплеск нагрузки при решении капчи.
	if strings.Contains(text, `"type":"session"`) && strings.Contains(text, `"code":4002`) {
		session.safeWrite(websocket.TextMessage,
			[]byte(`42["message",{"type":"extendSession","idletime":0}]`))
		utils.Debugf("[YDOCS] idle warning -> sent extendSession")
		return
	}

	// Держатель лока узнаёт о ждущем редакторе через broadcast connectState
	// с waitAuth:true (DocsCoServer.js sendParticipantsState). Пока держатель
	// не ответит {"type":"unLockDocument","unlock":true}, сервер держит
	// джойнера в waitAuth, а таймер принудительного анлока
	// (setLockDocumentTimer, ~60с) сбрасывается каждым новым джойном ждущего
	// (cleanLockDocumentTimer+setLockDocumentTimer) - без ответа цикл
	// вечный. Отвечаем немедленно; ответ от не-держателя безвреден:
	// unlockAuth сверяет userId и молча проваливается.
	if session.AuthConfirmed() &&
		strings.Contains(text, `"type":"connectState"`) &&
		strings.Contains(text, `"waitAuth":true`) {
		session.safeWrite(websocket.TextMessage,
			[]byte(`42["message",{"type":"unLockDocument","unlock":true}]`))
		utils.Debugf("[YDOCS] waitAuth broadcast -> sent unLockDocument")
		return
	}

	// Контрольные сообщения коллаборации (lockDocument и пр.) раньше
	// молча игнорировались - а именно на них надо отвечать unLockDocument,
	// иначе сервер выкидывает держателя лока через 60с (disconnectReason
	// 4007, см. DocsCoServer.js setLockDocumentTimer/checkEndAuthLock).
	if m := messageTypeRe.FindStringSubmatch(text); m != nil && m[1] != "cursor" {
		utils.Debugf("[YDOCS] control message: %s", shortStr(text, 400))
	}

	// Socket.IO ping - respond with pong (use safeWrite)
	if text == "2" {
		if session != nil && session.Conn != nil {
			session.safeWrite(websocket.TextMessage, []byte("3"))
		}
		return
	}
	if text == "3" {
		return
	}

	if strings.Contains(text, "saveChanges") || strings.Contains(text, "cursor") {
		base64Str := t.extractBase64String(text)
		if base64Str == "" {
			return
		}

		decoded, err := base64.StdEncoding.DecodeString(base64Str)
		if err != nil {
			utils.Debugf("[YDOCS] Base64 decode error: %v", err)
			return
		}

		t.RecordReceive(len(decoded))
		t.CallReceive(decoded)
	}
}

func (t *YandexDocsTransport) extractBase64String(response string) string {
	if strings.Contains(response, "saveChanges") {
		marker := `"excelAdditionalInfo":"`
		left := strings.Index(response, marker) + len(marker)
		if left < len(marker) {
			return ""
		}
		right := strings.Index(response[left:], `"`)
		if right == -1 {
			return ""
		}
		return response[left : left+right]
	}

	matches := cursorPayloadRe.FindStringSubmatch(response)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

func (t *YandexDocsTransport) scheduleReconnect(attempt int) {
	next := attempt + 1
	if !t.IsRunning() || next >= t.GetConfig().MaxReconnectAttempts {
		return
	}

	// Back off before retrying so a server that closes us immediately doesn't
	// turn into a tight connect/close loop (previously reconnect was instant).
	d := reconnectBackoff(next)
	utils.Debugf("[YDOCS] reconnecting in %v (attempt %d)", d, next)
	time.Sleep(d)
	if !t.IsRunning() {
		return
	}

	t.RecordReconnect()
	t.connectToDoc(next)
}

// reconnectBackoff returns an exponential backoff with jitter, capped at 30s.
//
// Each reconnect dials a brand new WebSocket, which the doc-collab server
// registers as a brand new participant in the doc's room regardless of
// client-side user-id reuse - a fast connect/close/reconnect loop piles up
// visible "ghost" participants quickly (confirmed by logging the server's
// participant-list messages during a failure streak). The floor here (was
// 500ms) is raised to slow that churn down; this doesn't change steady-state
// throughput since successful connects never hit backoff at all.
func reconnectBackoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	shift := n - 1
	if shift > 4 {
		shift = 4
	}
	d := 1500 * time.Millisecond * time.Duration(1<<uint(shift))
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	// add up to +50% jitter
	d += time.Duration(rand.Int63n(int64(d/2) + 1))
	return d
}

func (t *YandexDocsTransport) fetchDocInfo(url, userID string) (YandexDocsInfo, error) {
	// Reuse the transport-wide jar: cookies from a previously solved
	// captcha make Yandex skip the captcha on reconnects entirely.
	jar := t.jar
	if jar == nil { // zero-value transport (tests)
		jar = newPersistJar("")
	}
	// Браузерный TLS-фингерпринт: с обычным Go-клиентом Яндекс-антибот
	// после первой капчи присылает вторую (checkbox), см. browserclient.go.
	client := newBrowserHTTPClient(jar, 15*time.Second)

	ua := browserUserAgent

	// Явно следуем по редиректам (капчи удлиняют цепочку).
	currentURL := url
	var resp *http.Response
	var err error

	// Счётчик подряд идущих эскалаций до силуэтной капчи. Один 7.73 в
	// retpath — не приговор: ретрай оригинального URL часто проходит без
	// капчи. Подряд несколько — сессия/IP реально
	// помечены ботными, дальше сабмитить бессмысленно и вредно.
	escalations := 0

	for hop := 0; hop < 20; hop++ {
		utils.Debugf("[YDOCS] hop %d: GET %s", hop, shortStr(currentURL, 120))

		req, _ := http.NewRequest("GET", currentURL, nil)
		req.Header.Set("User-Agent", ua)
		resp, err = client.Do(req)
		if err != nil {
			return YandexDocsInfo{}, fmt.Errorf("GET %s: %w", currentURL, err)
		}

		utils.Debugf("[YDOCS]   status=%d location=%s",
			resp.StatusCode, shortStr(resp.Header.Get("Location"), 120))

		// 200 — дошли до документа
		if resp.StatusCode == 200 {
			break
		}

		// 3xx — редирект
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			if loc == "" {
				return YandexDocsInfo{}, fmt.Errorf("redirect without Location from %s", currentURL)
			}

			// Капча — проходим и повторяем ИСХОДНЫЙ url (не loc).
			if strings.Contains(loc, "showcaptchafast") {
				utils.Debugf("[YDOCS] captcha detected, solving...")
				if _, cerr := solveCaptcha(currentURL, jar, ua); cerr != nil {
					return YandexDocsInfo{}, fmt.Errorf("captcha solve: %w", cerr)
				}
				utils.Debugf("[YDOCS] captcha solved, retrying original url")
				currentURL = url
				continue
			}

			// Вторая, «checkbox»-капча (showcaptcha?cc=1) — тоже проходим.
			// Важно: после showcaptchafast (строка выше), т.к. "showcaptchafast"
			// содержит "showcaptcha" как подстроку.
			if strings.Contains(loc, "showcaptcha") {
				utils.Debugf("[YDOCS] checkbox captcha detected, solving...")
				retpath, cerr := solveCheckboxCaptcha(loc, jar, ua)
				if cerr != nil {
					return YandexDocsInfo{}, fmt.Errorf("checkbox captcha solve: %w", cerr)
				}
				// Эскалация до силуэтной капчи (form-fb-hint=7.73).
				// Единичный случай — продолжаем (ретрай может пройти);
				// несколько подряд — сессия/IP помечены ботными:
				// выходим, cookies отравлены, начинаем с чистого листа.
				if strings.Contains(retpath, "form-fb-hint=7.73") {
					escalations++
					if escalations >= 3 {
						jar.Clear()
						// Новая cookie-идентичность = новое "устройство":
						// перевыгенерим профиль отпечатка, чтобы не приносить
						// прожжённый fingerprint на свежие cookies.
						refreshDeviceProfile()
						return YandexDocsInfo{}, fmt.Errorf("captcha escalated to silhouette %d times (bot-flagged session/IP)", escalations)
					}
					utils.Debugf("[YDOCS] silhouette escalation #%d, retrying original url", escalations)
					currentURL = url
					continue
				}
				escalations = 0
				utils.Debugf("[YDOCS] checkbox captcha solved, retrying original url")
				currentURL = url
				continue
			}

			// Обычный редирект — идём по нему.
			currentURL = loc
			continue
		}

		// Другой статус — ошибка
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return YandexDocsInfo{}, fmt.Errorf("unexpected status %d at %s", resp.StatusCode, currentURL)
	}

	if resp == nil {
		return YandexDocsInfo{}, fmt.Errorf("no response after redirects")
	}
	if resp.StatusCode != 200 {
		return YandexDocsInfo{}, fmt.Errorf("final status %d", resp.StatusCode)
	}
	defer resp.Body.Close()

	htmlBytes, _ := io.ReadAll(resp.Body)
	html := string(htmlBytes)
	utils.Debugf("[YDOCS] response status=%d finalURL=%s body=%dB",
		resp.StatusCode, resp.Request.URL.String(), len(html))

	var cookies []string
	for _, c := range resp.Cookies() {
		cookies = append(cookies, fmt.Sprintf("%s=%s", c.Name, c.Value))
	}
	for _, c := range jar.Cookies(resp.Request.URL) {
		cookies = append(cookies, fmt.Sprintf("%s=%s", c.Name, c.Value))
	}

	matches := clientConfigRe.FindStringSubmatch(html)
	if len(matches) < 2 {
		hint := "no client-config script"
		if strings.Contains(html, "passport") || strings.Contains(strings.ToLower(html), "login") {
			hint = "looks like a login page (doc not public?)"
		}
		return YandexDocsInfo{}, fmt.Errorf("config not found: %s (status %d, final %s)",
			hint, resp.StatusCode, resp.Request.URL.String())
	}

	var config map[string]interface{}
	if err := json.Unmarshal([]byte(matches[1]), &config); err != nil {
		return YandexDocsInfo{}, fmt.Errorf("client-config is not valid JSON: %w", err)
	}

	officeAction, ok := config["officeActionData"].(map[string]interface{})
	if !ok || officeAction == nil {
		return YandexDocsInfo{}, fmt.Errorf("officeActionData missing - will reconnect")
	}

	editorConfigRaw, ok := officeAction["editor_config"].(map[string]interface{})
	if !ok || editorConfigRaw == nil {
		return YandexDocsInfo{}, fmt.Errorf("editor_config nil - will reconnect")
	}

	balancerURL, ok := officeAction["balancer_url"].(string)
	if !ok || balancerURL == "" {
		return YandexDocsInfo{}, fmt.Errorf("officeActionData.balancer_url missing - will reconnect")
	}
	host := strings.TrimPrefix(balancerURL, "https://")

	document, ok := editorConfigRaw["document"].(map[string]interface{})
	if !ok || document == nil {
		return YandexDocsInfo{}, fmt.Errorf("editor_config.document missing - will reconnect")
	}

	token, ok := editorConfigRaw["token"].(string)
	if !ok || token == "" {
		return YandexDocsInfo{}, fmt.Errorf("editor_config.token missing - will reconnect")
	}

	docKey, ok := document["key"].(string)
	if !ok || docKey == "" {
		return YandexDocsInfo{}, fmt.Errorf("editor_config.document.key missing - will reconnect")
	}

	perms, _ := document["permissions"].(map[string]interface{})
	if perms == nil {
		perms = make(map[string]interface{})
	}

	// Session reached the doc: the cookie set is trusted now. Persist it so
	// restarts/reconnects skip the captcha (each solve burns IP reputation).
	jar.Save()

	return YandexDocsInfo{
		CookieStr:   strings.Join(cookies, "; "),
		Token:       token,
		DocID:       docKey,
		Origin:      balancerURL,
		Host:        host,
		WsURL:       fmt.Sprintf("wss://%s/2024.1.1-375/doc/%s/c/?EIO=4&transport=websocket", host, docKey),
		Permissions: perms,
		OpenCmd: map[string]interface{}{
			"c":      "open",
			"id":     docKey,
			"userid": userID,
			"format": document["fileType"],
			"url":    document["url"],
			"title":  document["title"],
			"lcid":   25,
		},
	}, nil
}

func randUserID() string {
	return fmt.Sprintf("%010d", rand.New(rand.NewSource(time.Now().UnixNano())).Intn(1000000000))
}
