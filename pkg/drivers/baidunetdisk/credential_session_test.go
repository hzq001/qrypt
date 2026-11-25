package baidunetdisk

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yinzhenyu/qrypt/pkg/drive"
	"github.com/yinzhenyu/qrypt/pkg/drivers/internal/driverutil/credentialtest"
)

func TestSharedCredentialSessionIsAdopted(t *testing.T) {
	session := credentialtest.New("main")
	expires := time.Now().Add(time.Hour).UTC()
	payload, _, err := encodeCredentialPayload(credentialPayload{
		AccessToken:  "access-a",
		RefreshToken: "refresh-a",
		ExpiresAt:    expires,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Update(0, payload, ""); err != nil {
		t.Fatal(err)
	}
	driver := New(Options{RefreshToken: "config"})
	driver.InstallCredentialSession(session)
	driver.loadCredentialSession()

	if driver.accessToken != "access-a" || driver.refreshToken != "refresh-a" {
		t.Fatalf("tokens = %q/%q, want the shared session pair", driver.accessToken, driver.refreshToken)
	}
	if driver.tokenSource != "state" {
		t.Fatalf("source = %q, want state", driver.tokenSource)
	}
}

func TestExportRequiresConfigMarker(t *testing.T) {
	store := drive.NewFileStateStore(t.TempDir())
	if err := store.SaveJSON("baidu_netdisk_token.json", map[string]any{
		"access_token": "access", "refresh_token": "refresh",
	}); err != nil {
		t.Fatal(err)
	}
	driver := New(Options{RefreshToken: "config"})
	_, _, ok, err := driver.ExportCredentialSession(store, drive.Params{"refresh_token": "config"})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("state without config_refresh_token must not be adopted")
	}
}

func TestExportReturnsRecordedAccount(t *testing.T) {
	store := drive.NewFileStateStore(t.TempDir())
	if err := store.SaveJSON("baidu_netdisk_token.json", map[string]any{
		"access_token":         "access",
		"refresh_token":        "refresh",
		"account":              "baidu-user",
		"config_refresh_token": "config",
	}); err != nil {
		t.Fatal(err)
	}
	driver := New(Options{RefreshToken: "config"})
	_, account, ok, err := driver.ExportCredentialSession(store, drive.Params{"refresh_token": "config"})
	if err != nil {
		t.Fatal(err)
	}
	if !ok || account != "baidu-user" {
		t.Fatalf("export = ok:%v account:%q, want baidu-user", ok, account)
	}
}

func TestSharedCredentialForcedRefreshRotatesBeforeExpiry(t *testing.T) {
	t.Parallel()

	session := credentialtest.New("main")
	payload, _, err := encodeCredentialPayload(credentialPayload{
		AccessToken:  "access-a",
		RefreshToken: "refresh-a",
		ExpiresAt:    time.Now().Add(30 * time.Minute).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Update(0, payload, ""); err != nil {
		t.Fatal(err)
	}

	var refreshCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			refreshCalls.Add(1)
			if got := r.URL.Query().Get("refresh_token"); got != "refresh-a" {
				t.Errorf("refresh_token = %q, want refresh-a", got)
			}
			writeJSON(t, w, tokenResp{
				AccessToken:  "access-b",
				RefreshToken: "refresh-b",
				ExpiresIn:    3600,
			})
		case "/rest/2.0/xpan/multimedia":
			writeJSON(t, w, downloadResp{List: []struct {
				Dlink string `json:"dlink"`
			}{{Dlink: srvURL(r) + "/dlink"}}})
		case "/dlink":
			if r.URL.Query().Get("access_token") != "access-b" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Header().Set("Location", srvURL(r)+"/download")
			w.WriteHeader(http.StatusFound)
		case "/download":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("fresh-from-shared-refresh"))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	driver := New(Options{
		RefreshToken: "config",
		ClientID:     "client",
		ClientSecret: "secret",
		OAuthURL:     srv.URL + "/token",
		APIBaseURL:   srv.URL + "/rest/2.0",
		UseOnlineAPI: false,
	})
	driver.InstallCredentialSession(session)
	driver.loadCredentialSession()
	driver.subscribeCredentialSession()
	t.Cleanup(func() { _ = driver.Drop(context.Background()) })

	rc, err := driver.Read(context.Background(), driveEntry("/Qrypt/file.bin", 25, 9), 0, 0)
	if err != nil {
		t.Fatalf("Read() error = %v, want forced shared refresh to recover", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "fresh-from-shared-refresh" {
		t.Fatalf("read content = %q, want fresh-from-shared-refresh", string(data))
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("OAuth refresh calls = %d, want 1", got)
	}
	updated, err := decodeCredentialPayload(session.Snapshot().Payload)
	if err != nil {
		t.Fatal(err)
	}
	if updated.AccessToken != "access-b" || updated.RefreshToken != "refresh-b" {
		t.Fatalf("shared session payload = %+v, want access-b/refresh-b", updated)
	}
}
