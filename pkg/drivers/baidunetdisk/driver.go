package baidunetdisk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yinzhenyu/qrypt/pkg/drive"
	"github.com/yinzhenyu/qrypt/pkg/drive/session"
	"github.com/yinzhenyu/qrypt/pkg/drivers/internal/driverutil"
	"github.com/yinzhenyu/qrypt/pkg/drivers/internal/driverutil/httpclient"
	"github.com/yinzhenyu/qrypt/pkg/drivers/internal/driverutil/httputil"
	"github.com/yinzhenyu/qrypt/pkg/logging"
)

const (
	defaultAPIBaseURL    = "https://pan.baidu.com/rest/2.0"
	defaultOAuthURL      = "https://openapi.baidu.com/oauth/2.0/token"
	defaultOnlineAPI     = "https://api.oplist.org/baiduyun/renewapi"
	defaultUploadAPI     = "https://d.pcs.baidu.com"
	defaultRootPath      = "/"
	defaultDownloadUA    = "pan.baidu.com"
	defaultDownloadTTL   = time.Hour
	defaultTokenSkew     = 5 * time.Minute
	defaultListPageLimit = 1000
	defaultUploadPart    = 4 << 20
	maxUploadParts       = 2048
	firstSliceMD5Size    = 256 << 10
)

type Driver struct {
	drive.UnsupportedOperations
	httpClient         *http.Client
	downloadClient     *http.Client
	refreshToken       string
	accessToken        string
	account            string
	configRefreshToken string
	clientID           string
	clientSecret       string
	rootPath           string
	orderBy            string
	orderDesc          bool
	apiBaseURL         string
	oauthURL           string
	onlineAPI          string
	uploadAPI          string
	useOnlineAPI       bool
	downloadUA         string
	limiter            *drive.BandwidthLimiter
	stateStore         drive.StateStore
	// cred owns the shared-session lifecycle for mounts that reference a
	// [[credentials]] entry; a nil session keeps the legacy per-mount token
	// state.
	cred         *driverutil.CredentialBinding[credentialPayload]
	tokenSource  string
	tokenUpdated time.Time
	// refreshMu serializes direct OAuth refreshes; tokenMu guards in-memory
	// token/account fields only and must never be held across network or cred I/O.
	refreshMu          sync.Mutex
	tokenMu            sync.Mutex
	tokenExpires       time.Time
	downloadCache      sync.Map
	lastErrorMu        sync.Mutex
	lastError          string
	instantUploadCount int64
	metrics            *driverutil.Buffer

	// Upload session binding store：只保存 "内容键 → provider 上传句柄"，
	// 分片进度用 superfile2 list 从服务端重建。
	sessions       *session.Index
	sessionStoreMu sync.Mutex
	sessionCancel  context.CancelFunc
}

var errBaiduUploadIDExpired = errors.New("baidu_netdisk: uploadid expired")

type Options struct {
	RefreshToken string
	AccessToken  string
	ClientID     string
	ClientSecret string
	RootPath     string
	OrderBy      string
	OrderDesc    bool
	APIBaseURL   string
	OAuthURL     string
	OnlineAPI    string
	UploadAPI    string
	UseOnlineAPI bool
	DownloadUA   string
}

type cachedDownloadURL struct {
	URL       string
	ExpiresAt time.Time
}

type tokenState struct {
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	Account      string    `json:"account,omitempty"`
	// ConfigRefreshToken records the config refresh token that produced this
	// state, so a rotated pair (state newer than config) can be distinguished
	// from a user-swapped config token (config newer than state).
	ConfigRefreshToken string    `json:"config_refresh_token,omitempty"`
	UpdatedAt          time.Time `json:"updated_at,omitempty"`
}

const baiduSessionFile = "baidu_netdisk_upload_sessions.json"
const baiduSessionMaxAge = 24 * time.Hour
const baiduSessionExpiryEvery = time.Hour

func init() {
	drive.Register("baidu_netdisk", func(params drive.Params) (drive.Driver, error) {
		refreshToken := params["refresh_token"]
		if refreshToken == "" {
			return nil, fmt.Errorf("baidu_netdisk: missing refresh_token")
		}
		useOnlineAPI := true
		if raw := params["use_online_api"]; raw != "" {
			parsed, err := strconv.ParseBool(raw)
			if err != nil {
				return nil, fmt.Errorf("baidu_netdisk: invalid use_online_api: %w", err)
			}
			useOnlineAPI = parsed
		}
		orderDesc := false
		switch strings.ToLower(params["order_direction"]) {
		case "", "asc":
		case "desc":
			orderDesc = true
		default:
			return nil, fmt.Errorf("baidu_netdisk: order_direction must be asc or desc")
		}
		return New(Options{
			RefreshToken: refreshToken,
			AccessToken:  params["access_token"],
			ClientID:     params["client_id"],
			ClientSecret: params["client_secret"],
			RootPath:     params["root_path"],
			OrderBy:      params["order_by"],
			OrderDesc:    orderDesc,
			APIBaseURL:   params["api_base_url"],
			OAuthURL:     params["oauth_url"],
			OnlineAPI:    params["online_api"],
			UploadAPI:    params["upload_api"],
			UseOnlineAPI: useOnlineAPI,
			DownloadUA:   params["download_user_agent"],
		}), nil
	})
}

func New(opts Options) *Driver {
	rootPath := normalizeDir(opts.RootPath)
	if rootPath == "" {
		rootPath = defaultRootPath
	}
	apiBaseURL := strings.TrimRight(opts.APIBaseURL, "/")
	if apiBaseURL == "" {
		apiBaseURL = defaultAPIBaseURL
	}
	oauthURL := opts.OAuthURL
	if oauthURL == "" {
		oauthURL = defaultOAuthURL
	}
	onlineAPI := opts.OnlineAPI
	if onlineAPI == "" {
		onlineAPI = defaultOnlineAPI
	}
	uploadAPI := strings.TrimRight(opts.UploadAPI, "/")
	if uploadAPI == "" {
		uploadAPI = defaultUploadAPI
	}
	downloadUA := opts.DownloadUA
	if downloadUA == "" {
		downloadUA = defaultDownloadUA
	}
	downloadTransport := httputil.DefaultTransport(60 * time.Second)
	downloadTransport.DisableKeepAlives = true
	d := &Driver{
		httpClient:         httputil.NewClient(60*time.Second, 30*time.Second),
		downloadClient:     &http.Client{Transport: downloadTransport},
		refreshToken:       opts.RefreshToken,
		accessToken:        opts.AccessToken,
		configRefreshToken: opts.RefreshToken,
		clientID:           opts.ClientID,
		clientSecret:       opts.ClientSecret,
		rootPath:           rootPath,
		orderBy:            opts.OrderBy,
		orderDesc:          opts.OrderDesc,
		apiBaseURL:         apiBaseURL,
		oauthURL:           oauthURL,
		onlineAPI:          onlineAPI,
		uploadAPI:          uploadAPI,
		useOnlineAPI:       opts.UseOnlineAPI,
		downloadUA:         downloadUA,
		tokenSource:        "config",
		metrics:            driverutil.NewBuffer(500),
	}
	d.cred = driverutil.NewCredentialBinding("baidu_netdisk", decodeCredentialPayload, d.applyCredentialBinding)
	return d
}

func (d *Driver) Init(ctx context.Context) error {
	if _, refreshToken, _ := d.tokenStateSnapshot(); refreshToken == "" {
		return fmt.Errorf("baidu_netdisk: refresh_token is required")
	}
	if d.cred.Session() != nil {
		d.loadCredentialSession()
		d.subscribeCredentialSession()
	} else {
		d.loadTokenState()
	}
	if !d.useOnlineAPI && (d.clientID == "" || d.clientSecret == "") {
		return fmt.Errorf("baidu_netdisk: client_id and client_secret are required when use_online_api=false")
	}
	if access, _, expires := d.tokenStateSnapshot(); !tokenFresh(access, expires) {
		if err := d.refresh(ctx); err != nil {
			d.setLastError(err)
			return err
		}
	}
	d.loadAccount(ctx)
	if d.rootPath != "/" {
		if _, err := d.statRoot(ctx); err != nil {
			d.setLastError(err)
			return fmt.Errorf("baidu_netdisk: validate root_path %q: %w", d.rootPath, err)
		}
	}
	return nil
}

func (d *Driver) Drop(ctx context.Context) error {
	d.cred.Close()
	d.sessionStoreMu.Lock()
	if d.sessionCancel != nil {
		d.sessionCancel()
		d.sessionCancel = nil
	}
	d.sessionStoreMu.Unlock()
	if d.sessions != nil {
		_ = d.sessions.Flush()
	}
	return nil
}

func (d *Driver) InstallStateStore(store drive.StateStore) {
	d.stateStore = store
	d.installSessionIndex(store)
}

// InstallCredentialSession lets every mount of one configured Baidu credential
// share one token pair and coalesce refreshes.
func (d *Driver) InstallCredentialSession(session drive.CredentialSession) {
	d.cred.Install(session)
}

func (d *Driver) InstallBandwidthLimiter(limiter *drive.BandwidthLimiter) drive.BandwidthLimitDirection {
	d.limiter = limiter
	return drive.BandwidthLimitDownload | drive.BandwidthLimitUpload
}

func (d *Driver) metricEvents(ctx context.Context, since time.Time) ([]drive.MetricEvent, error) {
	return d.metrics.Events(since), nil
}

func (d *Driver) DebugSnapshot(ctx context.Context) (drive.DebugSnapshot, error) {
	d.lastErrorMu.Lock()
	lastError := d.lastError
	instantUploadCount := d.instantUploadCount
	d.lastErrorMu.Unlock()
	health := "ok"
	if lastError != "" {
		health = "degraded"
	}
	tokenSource, tokenUpdated := d.tokenDebugMeta()
	extra := map[string]any{
		drive.DebugExtraCredentialSource:   tokenSource,
		drive.DebugExtraCredentialUpdated:  tokenUpdated,
		drive.DebugExtraLastError:          lastError,
		drive.DebugExtraInstantUploadCount: instantUploadCount,
	}
	d.cred.Debug(extra)
	return drive.DebugSnapshot{
		Driver:      "baidu_netdisk",
		Health:      health,
		GeneratedAt: time.Now(),
		Stats: map[string]any{
			drive.DebugStatRootPath: d.rootPath,
			"order_by":              d.orderBy,
			"order_desc":            d.orderDesc,
			"use_online_api":        d.useOnlineAPI,
			"upload_api":            d.uploadAPI,
		},
		Extra: extra,
	}, nil
}

func (d *Driver) Space(ctx context.Context) (drive.Space, error) {
	var resp quotaResp
	if err := d.request(ctx, http.MethodGet, "https://pan.baidu.com/api/quota", nil, nil, &resp); err != nil {
		err = fmt.Errorf("baidu_netdisk: space: %w", err)
		d.setLastError(err)
		return drive.Space{}, err
	}
	return drive.Space{Total: resp.Total, Free: resp.Total - resp.Used}, nil
}

// RemoteHash reports the content MD5 the API returned when the entry was
// listed (baidu computes md5 of the stored bytes; large files may lack it).

func (d *Driver) ResolvePath(ctx context.Context, p string) (string, error) {
	if p == "" || p == "/" {
		return d.rootPath, nil
	}
	return normalizeDir(path.Join(d.rootPath, strings.Trim(p, "/"))), nil
}

func (d *Driver) setLastError(err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	d.lastErrorMu.Lock()
	defer d.lastErrorMu.Unlock()
	if err == nil {
		d.lastError = ""
		return
	}
	d.lastError = err.Error()
}

type apiError struct {
	errno   int
	message string
}

func (d *Driver) recordHTTP(ctx context.Context, operation string, req *http.Request, resp *http.Response, start time.Time, request map[string]any, err error) {
	event := drive.MetricEvent{
		Operation: operation,
		Method:    req.Method,
		URL:       driverutil.URL(req.URL),
		Duration:  time.Since(start).String(),
		Request:   request,
	}
	if resp != nil {
		event.Status = resp.StatusCode
	}
	if err != nil {
		event.Error = err.Error()
	}
	d.metrics.Record(ctx, event)
}

func tokenFresh(access string, expires time.Time) bool {
	return access != "" && !expires.IsZero() && time.Now().Before(expires.Add(-defaultTokenSkew))
}

func canReuseToken(access string, expires time.Time, force bool, failedToken string) bool {
	if !tokenFresh(access, expires) {
		return false
	}
	if !force {
		return true
	}
	return failedToken != "" && access != failedToken
}

func (d *Driver) currentAccessToken() string {
	d.tokenMu.Lock()
	defer d.tokenMu.Unlock()
	return d.accessToken
}

func (d *Driver) tokenStateSnapshot() (accessToken, refreshToken string, expiresAt time.Time) {
	d.tokenMu.Lock()
	defer d.tokenMu.Unlock()
	return d.accessToken, d.refreshToken, d.tokenExpires
}

func (d *Driver) tokenDebugMeta() (source string, updated time.Time) {
	d.tokenMu.Lock()
	defer d.tokenMu.Unlock()
	return d.tokenSource, d.tokenUpdated
}

func (d *Driver) credentialState() (access, refresh string, expires time.Time, account string) {
	d.tokenMu.Lock()
	defer d.tokenMu.Unlock()
	return d.accessToken, d.refreshToken, d.tokenExpires, d.account
}

func (d *Driver) ensureToken(ctx context.Context) error {
	access, _, expires := d.tokenStateSnapshot()
	if !tokenFresh(access, expires) {
		return d.refresh(ctx)
	}
	return nil
}

func (d *Driver) refresh(ctx context.Context) error {
	return d.refreshWithMode(ctx, false, "")
}

func (d *Driver) refreshForced(ctx context.Context, failedToken string) error {
	return d.refreshWithMode(ctx, true, failedToken)
}

func (d *Driver) refreshWithMode(ctx context.Context, force bool, failedToken string) error {
	if d.cred.Session() != nil {
		return d.refreshShared(ctx, force, failedToken)
	}
	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()
	access, _, expires := d.tokenStateSnapshot()
	if canReuseToken(access, expires, force, failedToken) {
		return nil
	}
	return d.refreshDirectLocked(ctx)
}

func (d *Driver) refreshDirect(ctx context.Context) error {
	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()
	return d.refreshDirectLocked(ctx)
}

// refreshDirectLocked requires d.refreshMu and must never hold d.tokenMu across
// network or state-store I/O so concurrent readers never block or deadlock.
func (d *Driver) refreshDirectLocked(ctx context.Context) error {
	_, refreshToken, _ := d.tokenStateSnapshot()
	if refreshToken == "" {
		return fmt.Errorf("baidu_netdisk: refresh_token is required")
	}
	var resp tokenResp
	if d.useOnlineAPI {
		u, err := url.Parse(d.onlineAPI)
		if err != nil {
			return err
		}
		query := u.Query()
		query.Set("refresh_ui", refreshToken)
		query.Set("server_use", "true")
		query.Set("driver_txt", "baiduyun_go")
		u.RawQuery = query.Encode()
		if err := d.requestToken(ctx, http.MethodGet, u.String(), nil, &resp); err != nil {
			return fmt.Errorf("baidu_netdisk: refresh token via online_api: %w; if this is a normal Baidu OAuth refresh token, set use_online_api=false and configure client_id/client_secret", err)
		}
	} else {
		form := url.Values{}
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", refreshToken)
		form.Set("client_id", d.clientID)
		form.Set("client_secret", d.clientSecret)
		if err := d.requestToken(ctx, http.MethodGet, d.oauthURL+"?"+form.Encode(), nil, &resp); err != nil {
			return fmt.Errorf("baidu_netdisk: refresh token: %w", err)
		}
	}
	if resp.Error != "" {
		if resp.Error == "invalid_client" {
			return fmt.Errorf("baidu_netdisk: refresh token: %s: %s; client_id must be the Baidu app API Key and client_secret must be the app Secret Key", resp.Error, resp.ErrorDesc)
		}
		return fmt.Errorf("baidu_netdisk: refresh token: %s: %s", resp.Error, resp.ErrorDesc)
	}
	if resp.AccessToken == "" || resp.RefreshToken == "" {
		if resp.ErrorMessage != "" {
			if d.useOnlineAPI {
				return fmt.Errorf("baidu_netdisk: refresh token via online_api: %s; if this is a normal Baidu OAuth refresh token, set use_online_api=false and configure client_id/client_secret", resp.ErrorMessage)
			}
			return fmt.Errorf("baidu_netdisk: refresh token: %s", resp.ErrorMessage)
		}
		return fmt.Errorf("baidu_netdisk: refresh token returned empty token")
	}
	expires := time.Now().Add(time.Hour)
	if resp.ExpiresIn > 0 {
		expires = time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second)
	}
	updated := time.Now()
	d.tokenMu.Lock()
	d.accessToken = resp.AccessToken
	d.refreshToken = resp.RefreshToken
	d.tokenExpires = expires
	d.tokenSource = "refresh"
	d.tokenUpdated = updated
	state := tokenState{
		AccessToken:        d.accessToken,
		RefreshToken:       d.refreshToken,
		ExpiresAt:          d.tokenExpires,
		Account:            d.account,
		ConfigRefreshToken: d.configRefreshToken,
		UpdatedAt:          d.tokenUpdated,
	}
	d.tokenMu.Unlock()

	if d.cred.Session() != nil {
		return nil
	}
	if err := d.saveTokenStateSnapshot(state); err != nil {
		return fmt.Errorf("baidu_netdisk: save token state: %w", err)
	}
	return nil
}

// refreshShared coalesces refreshes across every mount sharing the credential.
// The session's refresh token is authoritative so a rotated chain is not lost.
func (d *Driver) refreshShared(ctx context.Context, force bool, failedToken string) error {
	_, err := d.cred.Refresh(ctx, func(value credentialPayload, current drive.CredentialSnapshot) ([]byte, string, error) {
		d.tokenMu.Lock()
		if value.RefreshToken != "" {
			d.refreshToken = value.RefreshToken
		}
		if value.AccessToken != "" {
			d.accessToken = value.AccessToken
			d.tokenExpires = value.ExpiresAt
		}
		access := d.accessToken
		expires := d.tokenExpires
		account := d.account
		d.tokenMu.Unlock()

		if canReuseToken(access, expires, force, failedToken) {
			return current.Payload, account, nil
		}
		if refreshErr := d.refreshDirect(ctx); refreshErr != nil {
			return nil, "", refreshErr
		}
		access, refresh, expires, account := d.credentialState()
		encoded, _, encodeErr := encodeCredentialPayload(credentialPayload{
			AccessToken:  access,
			RefreshToken: refresh,
			ExpiresAt:    expires,
		})
		return encoded, account, encodeErr
	})
	return err
}

// loadCredentialSession adopts the shared session's token pair and account.
func (d *Driver) loadCredentialSession() {
	d.cred.Load()
}

// subscribeCredentialSession follows rotations committed by sibling mounts.
func (d *Driver) subscribeCredentialSession() {
	d.cred.Subscribe()
}

// applyCredentialBinding applies one decoded payload to the client.
func (d *Driver) applyCredentialBinding(value credentialPayload, snapshot drive.CredentialSnapshot, source string) bool {
	if value.AccessToken == "" && value.RefreshToken == "" {
		return false
	}
	d.tokenMu.Lock()
	d.accessToken = value.AccessToken
	d.refreshToken = value.RefreshToken
	d.tokenExpires = value.ExpiresAt
	d.tokenSource = source
	d.tokenUpdated = snapshot.UpdatedAt
	if snapshot.Account != "" {
		d.account = snapshot.Account
	}
	d.tokenMu.Unlock()
	return true
}

type credentialPayload struct {
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
}

func encodeCredentialPayload(payload credentialPayload) ([]byte, string, error) {
	data, err := json.Marshal(payload)
	return data, "", err
}

func decodeCredentialPayload(data []byte) (credentialPayload, error) {
	var payload credentialPayload
	if len(data) == 0 {
		return payload, nil
	}
	err := json.Unmarshal(data, &payload)
	return payload, err
}

// loadAccount records a human-readable account name for diagnostics. It is
// best-effort: a provider or online-api hiccup must not fail the mount.
func (d *Driver) loadAccount(ctx context.Context) {
	var resp struct {
		Errno       int    `json:"errno"`
		BaiduName   string `json:"baidu_name"`
		NetdiskName string `json:"netdisk_name"`
		UK          int64  `json:"uk"`
	}
	if err := d.request(ctx, http.MethodGet, d.apiBaseURL+"/xpan/nas", map[string]string{"method": "uinfo"}, nil, &resp); err != nil {
		logging.L.Warnf("[baidu_netdisk] fetch account: %v", err)
		return
	}
	var account string
	switch {
	case resp.NetdiskName != "":
		account = resp.NetdiskName
	case resp.BaiduName != "":
		account = resp.BaiduName
	case resp.UK != 0:
		account = strconv.FormatInt(resp.UK, 10)
	}
	if account != "" {
		d.tokenMu.Lock()
		d.account = account
		d.tokenMu.Unlock()
	}
}

func (d *Driver) loadTokenState() {
	if d.stateStore == nil {
		return
	}
	var state tokenState
	err := d.stateStore.LoadJSON("baidu_netdisk_token.json", &state)
	if err != nil {
		if !drive.IsStateNotExist(err) {
			d.setLastError(fmt.Errorf("baidu_netdisk: load token state: %w", err))
		}
		return
	}
	// The state wins when it was derived from the current config token (token
	// rotation) or when it predates the source marker. A config token that
	// differs from the token the state was derived from is an account switch.
	stateDerived := state.ConfigRefreshToken == "" || state.ConfigRefreshToken == d.configRefreshToken
	d.tokenMu.Lock()
	if state.RefreshToken != "" && stateDerived {
		d.refreshToken = state.RefreshToken
		d.tokenSource = "state"
	}
	if state.AccessToken != "" && stateDerived {
		d.accessToken = state.AccessToken
	}
	if !state.ExpiresAt.IsZero() && stateDerived {
		d.tokenExpires = state.ExpiresAt
	}
	d.tokenUpdated = state.UpdatedAt
	if state.Account != "" {
		d.account = state.Account
	}
	d.tokenMu.Unlock()
}

func (d *Driver) saveTokenStateSnapshot(state tokenState) error {
	if d.stateStore == nil {
		return nil
	}
	return d.stateStore.SaveJSON("baidu_netdisk_token.json", state)
}

func (d *Driver) requestToken(ctx context.Context, method, rawURL string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return err
	}
	start := time.Now()
	resp, err := d.httpClient.Do(req)
	d.recordHTTP(ctx, "token", req, resp, start, nil, err)
	if err != nil {
		return err
	}
	data, err := httpclient.ReadBody(resp)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return drive.HTTPError("baidu_netdisk: token", req, resp, data)
	}
	return httpclient.DecodeJSON(data, out)
}

func (d *Driver) manage(ctx context.Context, op string, filelist any) error {
	data, err := json.Marshal(filelist)
	if err != nil {
		return err
	}
	return d.postForm(ctx, "/xpan/file", map[string]string{"method": "filemanager", "opera": op}, map[string]string{
		"async":    "0",
		"filelist": string(data),
		"ondup":    "fail",
	}, nil)
}

func (d *Driver) statRoot(ctx context.Context) (drive.Entry, error) {
	parent := path.Dir(d.rootPath)
	name := path.Base(d.rootPath)
	entries, err := d.listDir(ctx, parent)
	if err != nil {
		return drive.Entry{}, err
	}
	for _, entry := range entries {
		if entry.Name == name && entry.IsDir {
			return entry, nil
		}
	}
	return drive.Entry{}, fmt.Errorf("%w: path not found", drive.ErrNotFound)
}

func normalizeDir(p string) string {
	if p == "" {
		return ""
	}
	cleaned := path.Clean("/" + strings.TrimSpace(p))
	if cleaned == "." {
		return "/"
	}
	return cleaned
}

func baseName(p string) string {
	if p == "" || p == "/" {
		return ""
	}
	return path.Base(p)
}

var _ drive.Driver = (*Driver)(nil)
var _ drive.StateStoreInstaller = (*Driver)(nil)
var _ drive.BandwidthLimitInstaller = (*Driver)(nil)

func (e apiError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("baidu api errno %d", e.errno)
	}
	return fmt.Sprintf("baidu api errno %d: %s", e.errno, e.message)
}

func tokenExpired(err error) bool {
	var apiErr apiError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.errno == 111 || apiErr.errno == -6
}

func responseErrno(data []byte) (int, string) {
	var resp struct {
		Errno  *int   `json:"errno"`
		Errmsg string `json:"errmsg"`
	}
	if err := json.Unmarshal(data, &resp); err != nil || resp.Errno == nil {
		return 0, ""
	}
	return *resp.Errno, resp.Errmsg
}
