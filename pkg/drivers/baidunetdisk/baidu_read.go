package baidunetdisk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yinzhenyu/qrypt/pkg/drive"
)

type downloadStatusError struct {
	stage  string
	status int
}

func (e *downloadStatusError) Error() string {
	return fmt.Sprintf("baidu_netdisk: %s status %d", e.stage, e.status)
}

func isAuthStatusError(err error) bool {
	var statusErr *downloadStatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	return statusErr.status == http.StatusUnauthorized || statusErr.status == http.StatusForbidden
}

func (d *Driver) Read(ctx context.Context, entry drive.Entry, offset, size int64) (io.ReadCloser, error) {
	if offset < 0 || size < 0 {
		return nil, fmt.Errorf("baidu_netdisk: read: negative offset or size")
	}
	if entry.Size > 0 && offset >= entry.Size {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	for attempt := range 2 {
		usedToken := d.currentAccessToken()
		u, err := d.downloadURL(ctx, entry)
		if err != nil {
			if attempt == 0 && ctx.Err() == nil && isAuthStatusError(err) {
				d.downloadCache.Delete(entry.ID)
				_ = d.refreshForced(ctx, usedToken)
				continue
			}
			d.setLastError(err)
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", d.downloadUA)
		if offset > 0 || size > 0 {
			end := ""
			if size > 0 {
				endOffset := offset + size - 1
				if entry.Size > 0 && endOffset >= entry.Size {
					endOffset = entry.Size - 1
				}
				end = strconv.FormatInt(endOffset, 10)
			}
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%s", offset, end))
		}
		start := time.Now()
		resp, err := d.downloadClient.Do(req)
		d.recordHTTP(ctx, "download", req, resp, start, map[string]any{"range": req.Header.Get("Range")}, err)
		if err != nil {
			d.downloadCache.Delete(entry.ID)
			if attempt == 0 && ctx.Err() == nil {
				continue
			}
			err = fmt.Errorf("baidu_netdisk: read: %w", err)
			d.setLastError(err)
			return nil, err
		}
		if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && entry.Size > 0 && offset >= entry.Size {
			resp.Body.Close()
			return io.NopCloser(bytes.NewReader(nil)), nil
		}
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			d.downloadCache.Delete(entry.ID)
			if attempt == 0 && ctx.Err() == nil {
				_ = d.refreshForced(ctx, usedToken)
				continue
			}
			err := &downloadStatusError{stage: "read", status: resp.StatusCode}
			d.setLastError(err)
			return nil, err
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			d.downloadCache.Delete(entry.ID)
			err := &downloadStatusError{stage: "read", status: resp.StatusCode}
			d.setLastError(err)
			return nil, err
		}
		return d.limiter.LimitDownload(ctx, resp.Body), nil
	}
	return nil, fmt.Errorf("baidu_netdisk: read failed")
}

func (d *Driver) downloadURL(ctx context.Context, entry drive.Entry) (string, error) {
	if cached, ok := d.downloadCache.Load(entry.ID); ok {
		if item, ok := cached.(cachedDownloadURL); ok && item.URL != "" && time.Now().Before(item.ExpiresAt) {
			return item.URL, nil
		}
		d.downloadCache.Delete(entry.ID)
	}
	fsID := entryFSID(entry)
	if fsID == "" {
		return "", fmt.Errorf("baidu_netdisk: missing fs_id for %q", entry.ID)
	}
	var resp downloadResp
	if err := d.get(ctx, "/xpan/multimedia", map[string]string{
		"method": "filemetas",
		"fsids":  "[" + fsID + "]",
		"dlink":  "1",
	}, &resp); err != nil {
		return "", fmt.Errorf("baidu_netdisk: download url: %w", err)
	}
	if len(resp.List) == 0 || resp.List[0].Dlink == "" {
		return "", fmt.Errorf("baidu_netdisk: download url is empty")
	}
	dlink := resp.List[0].Dlink
	accessToken := d.currentAccessToken()
	if strings.Contains(dlink, "?") {
		dlink += "&access_token=" + url.QueryEscape(accessToken)
	} else {
		dlink += "?access_token=" + url.QueryEscape(accessToken)
	}
	redirectURL, err := d.resolveDownloadRedirect(ctx, dlink)
	if err != nil {
		return "", err
	}
	d.downloadCache.Store(entry.ID, cachedDownloadURL{URL: redirectURL, ExpiresAt: time.Now().Add(defaultDownloadTTL - defaultTokenSkew)})
	return redirectURL, nil
}

func (d *Driver) resolveDownloadRedirect(ctx context.Context, u string) (string, error) {
	client := *d.downloadClient
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	location, status, err := d.probeDownloadRedirect(ctx, &client, http.MethodHead, u, "")
	if err != nil {
		return "", err
	}
	if location != "" {
		return location, nil
	}
	if status >= 200 && status < 300 {
		return u, nil
	}
	if status == http.StatusForbidden || status == http.StatusMethodNotAllowed {
		location, status, err = d.probeDownloadRedirect(ctx, &client, http.MethodGet, u, "bytes=0-0")
		if err != nil {
			return "", err
		}
		if location != "" {
			return location, nil
		}
		if status == http.StatusOK || status == http.StatusPartialContent {
			return u, nil
		}
	}
	return "", &downloadStatusError{stage: "download redirect", status: status}
}

func (d *Driver) probeDownloadRedirect(ctx context.Context, client *http.Client, method, u, rangeHeader string) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", d.downloadUA)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	start := time.Now()
	resp, err := client.Do(req)
	d.recordHTTP(ctx, "resolve_download_redirect", req, resp, start, nil, err)
	if err != nil {
		return "", 0, fmt.Errorf("baidu_netdisk: download redirect: %w", err)
	}
	location := resp.Header.Get("Location")
	status := resp.StatusCode
	if location != "" {
		if parsed, err := url.Parse(location); err == nil && !parsed.IsAbs() {
			location = resp.Request.URL.ResolveReference(parsed).String()
		}
	}
	resp.Body.Close()
	return location, status, nil
}

func (d *Driver) RemoteHash(_ context.Context, entry drive.Entry) (drive.HashAlgorithm, string, error) {
	return drive.RemoteHashFromExtra(entry, "md5", drive.HashMD5)
}

// RemoteHashInfo reports the provider-computed MD5 from the listing.
func (d *Driver) RemoteHashInfo(ctx context.Context, entry drive.Entry) (drive.RemoteHashInfo, error) {
	algorithm, hash, err := d.RemoteHash(ctx, entry)
	if err != nil {
		return drive.RemoteHashInfo{}, err
	}
	return drive.RemoteHashInfo{Algorithm: algorithm, Hash: hash, Verified: true, Source: drive.RemoteHashSourceProvider}, nil
}

func (d *Driver) resolvePath(id string) string {
	if id == "" || id == "/" || id == "0" {
		return d.rootPath
	}
	return normalizeDir(id)
}

func (d *Driver) get(ctx context.Context, pathname string, params map[string]string, out any) error {
	return d.request(ctx, http.MethodGet, d.apiBaseURL+pathname, params, nil, out)
}
