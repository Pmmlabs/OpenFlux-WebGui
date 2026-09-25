package yandex

import (
	"os"
	"testing"

	"openflux/utils"
)

// Live check: fetchDocInfo must pass both captchas (showcaptchafast and the
// checkbox showcaptcha?cc=1) and reach the document's client-config.
// Run with: go test ./transport/yandex/ -run TestFetchDocInfoLive -v
func TestFetchDocInfoLive(t *testing.T) {
	if os.Getenv("OPENFLUX_LIVE_TEST") == "" {
		t.Skip("set OPENFLUX_LIVE_TEST=1 to run against the real doc")
	}
	utils.EnableDebug()

	docURL := os.Getenv("OPENFLUX_DOC_URL")
	if docURL == "" {
		docURL = "https://disk.yandex.ru/i/OLOLO" // замените на свой адрес документа
	}

	tr := &YandexDocsTransport{}
	info, err := tr.fetchDocInfo(docURL, randUserID())
	if err != nil {
		t.Fatalf("fetchDocInfo: %v", err)
	}
	t.Logf("doc info OK: %+v", info)
}
