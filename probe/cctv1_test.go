package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

const probeUserAgent = "okhttp/4.9.3"
const probeReferer = "https://m.miguvideo.com/"

type channelProbe struct {
	name string
	pid  string
}

var channels = []channelProbe{
	{name: "CCTV-2", pid: "631780532"},
	{name: "CCTV-5", pid: "641886683"},
	{name: "CCTV-13", pid: "608807423"},
}

func probeGet(client *http.Client, rawURL string, rangeHeader string) (*http.Response, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil { return nil, nil, err }
	req.Header.Set("User-Agent", probeUserAgent)
	req.Header.Set("Referer", probeReferer)
	if rangeHeader != "" { req.Header.Set("Range", rangeHeader) }
	resp, err := client.Do(req)
	if err != nil { return nil, nil, err }
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	resp.Body.Close()
	if readErr != nil { return resp, nil, readErr }
	return resp, body, nil
}

func firstPlaylistURI(body []byte) (string, error) {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") { return line, nil }
	}
	return "", fmt.Errorf("playlist_has_no_media_uri")
}

func isM3U8(body []byte) bool { return bytes.Contains(body, []byte("#EXTM3U")) }

func hostOnly(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil { return "unknown" }
	return u.Host
}

func TestCCTV1Probe(t *testing.T) {
	client := &http.Client{Timeout: 20 * time.Second}
	passed := 0

	for _, ch := range channels {
		t.Run(ch.name, func(t *testing.T) {
			started := time.Now()
			streamURL, err := GetAndroidURL(ch.pid, 3, "", "")
			if err != nil {
				t.Errorf("RESULT=%s resolve_failed error_type=%T", ch.name, err)
				return
			}
			u, err := url.Parse(streamURL)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				t.Errorf("RESULT=%s resolve_invalid", ch.name)
				return
			}
			t.Logf("RESULT=%s resolve_ok scheme=%s host=%s", ch.name, u.Scheme, u.Host)

			resp, manifest, err := probeGet(client, streamURL, "")
			if err != nil {
				t.Errorf("RESULT=%s playlist_request_failed error_type=%T", ch.name, err)
				return
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 || !isM3U8(manifest) {
				t.Errorf("RESULT=%s playlist_invalid status=%d m3u8_signature=%t bytes=%d",
					ch.name, resp.StatusCode, isM3U8(manifest), len(manifest))
				return
			}
			finalURL := streamURL
			if resp.Request != nil && resp.Request.URL != nil { finalURL = resp.Request.URL.String() }
			t.Logf("RESULT=%s playlist_ok status=%d content_type=%q host=%s bytes=%d",
				ch.name, resp.StatusCode, resp.Header.Get("Content-Type"), hostOnly(finalURL), len(manifest))

			mediaURI, err := firstPlaylistURI(manifest)
			if err != nil {
				t.Errorf("RESULT=%s media_uri_missing", ch.name)
				return
			}
			base, err := url.Parse(finalURL)
			if err != nil { t.Errorf("RESULT=%s playlist_base_invalid", ch.name); return }
			ref, err := url.Parse(mediaURI)
			if err != nil { t.Errorf("RESULT=%s media_uri_invalid", ch.name); return }
			mediaURL := base.ResolveReference(ref).String()

			for depth := 0; depth < 2; depth++ {
				refURL, _ := url.Parse(mediaURL)
				if refURL == nil || !strings.Contains(strings.ToLower(refURL.Path), ".m3u8") { break }
				nestedResp, nestedBody, nestedErr := probeGet(client, mediaURL, "")
				if nestedErr != nil || nestedResp.StatusCode < 200 || nestedResp.StatusCode >= 300 || !isM3U8(nestedBody) {
					t.Errorf("RESULT=%s nested_playlist_failed", ch.name)
					return
				}
				nestedBase := mediaURL
				if nestedResp.Request != nil && nestedResp.Request.URL != nil { nestedBase = nestedResp.Request.URL.String() }
				nextURI, nextErr := firstPlaylistURI(nestedBody)
				if nextErr != nil { t.Errorf("RESULT=%s nested_media_uri_missing", ch.name); return }
				b, _ := url.Parse(nestedBase)
				r, _ := url.Parse(nextURI)
				mediaURL = b.ResolveReference(r).String()
			}

			segResp, segBody, segErr := probeGet(client, mediaURL, "bytes=0-1023")
			if segErr != nil {
				t.Errorf("RESULT=%s segment_request_failed error_type=%T", ch.name, segErr)
				return
			}
			ct := strings.ToLower(segResp.Header.Get("Content-Type"))
			if (segResp.StatusCode != http.StatusOK && segResp.StatusCode != http.StatusPartialContent) ||
				len(segBody) == 0 || strings.Contains(ct, "text/html") || isM3U8(segBody) {
				t.Errorf("RESULT=%s segment_invalid status=%d content_type=%q bytes=%d",
					ch.name, segResp.StatusCode, ct, len(segBody))
				return
			}
			segmentHost := ""
			if segResp.Request != nil && segResp.Request.URL != nil { segmentHost = segResp.Request.URL.Host }
			t.Logf("RESULT=%s segment_ok status=%d content_type=%q host=%s sampled_bytes=%d elapsed_ms=%d",
				ch.name, segResp.StatusCode, ct, segmentHost, len(segBody), time.Since(started).Milliseconds())
			passed++
		})
	}
	if passed != len(channels) {
		t.Errorf("RESULT=CHANNELS_PARTIAL passed=%d total=%d", passed, len(channels))
		return
	}
	t.Logf("RESULT=ALL_CHANNELS_PASS total=%d", passed)
}
