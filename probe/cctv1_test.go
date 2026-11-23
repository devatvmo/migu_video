package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"
)

const probeUserAgent = "okhttp/4.9.3"
const probeReferer = "https://m.miguvideo.com/"

func summarizeResolverError(err error) string {
	msg := err.Error()
	if strings.HasPrefix(msg, "empty play url") {
		parts := strings.SplitN(msg, " response=", 2)
		rid := strings.TrimPrefix(parts[0], "empty play url rid=")
		summary := "api_response_missing_play_url rid=" + rid
		if len(parts) == 2 {
			var payload map[string]json.RawMessage
			if json.Unmarshal([]byte(parts[1]), &payload) == nil {
				keys := make([]string, 0, len(payload))
				for key := range payload {
					keys = append(keys, key)
				}
				sort.Strings(keys)
				summary += " top_level_keys=" + strings.Join(keys, ",")
				if raw, ok := payload["body"]; ok {
					var body map[string]json.RawMessage
					if json.Unmarshal(raw, &body) == nil {
						bodyKeys := make([]string, 0, len(body))
						for key := range body {
							bodyKeys = append(bodyKeys, key)
						}
						sort.Strings(bodyKeys)
						summary += " body_keys=" + strings.Join(bodyKeys, ",")
					}
				}
			} else {
				summary += " response_json=unparseable"
			}
		}
		return summary
	}
	return fmt.Sprintf("resolver_error_type=%T", err)
}

func newProbeClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

func probeGet(client *http.Client, rawURL string, rangeHeader string) (*http.Response, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", probeUserAgent)
	req.Header.Set("Referer", probeReferer)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	resp.Body.Close()
	if readErr != nil {
		return resp, nil, readErr
	}
	return resp, body, nil
}

func firstPlaylistURI(body []byte) (string, error) {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line, nil
	}
	return "", fmt.Errorf("playlist_has_no_media_uri")
}

func hostOnly(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "unknown"
	}
	return u.Host
}

func isM3U8(body []byte) bool {
	return bytes.Contains(body, []byte("#EXTM3U"))
}

// TestCCTV1Probe checks the first media object, then reuses the exact same
// temporary playback URL after 1 and 5 minutes. URLs and query tokens stay
// in memory and are never written to logs or artifacts.
func TestCCTV1Probe(t *testing.T) {
	const pid = "608807420"
	started := time.Now()

	streamURL, err := GetAndroidURL(pid, 3, "", "")
	if err != nil {
		t.Fatalf("RESULT=resolve_failed elapsed_ms=%d %s",
			time.Since(started).Milliseconds(), summarizeResolverError(err))
	}
	parsed, err := url.Parse(streamURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		t.Fatalf("RESULT=resolve_invalid_url elapsed_ms=%d", time.Since(started).Milliseconds())
	}
	issuedAt := time.Now()
	t.Logf("RESULT=resolve_ok scheme=%s host=%s", parsed.Scheme, parsed.Host)

	client := newProbeClient(20 * time.Second)
	resp, manifest, err := probeGet(client, streamURL, "")
	if err != nil {
		t.Fatalf("RESULT=initial_playlist_request_failed error_type=%T", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !isM3U8(manifest) {
		t.Fatalf("RESULT=initial_playlist_invalid status=%d m3u8_signature=%t body_bytes=%d",
			resp.StatusCode, isM3U8(manifest), len(manifest))
	}
	finalPlaylistURL := streamURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalPlaylistURL = resp.Request.URL.String()
	}
	t.Logf("RESULT=initial_playlist_ok status=%d content_type=%q final_host=%s body_bytes=%d",
		resp.StatusCode, resp.Header.Get("Content-Type"), hostOnly(finalPlaylistURL), len(manifest))

	mediaURI, err := firstPlaylistURI(manifest)
	if err != nil {
		t.Fatalf("RESULT=media_uri_missing")
	}
	playlistBase, err := url.Parse(finalPlaylistURL)
	if err != nil {
		t.Fatalf("RESULT=playlist_base_invalid")
	}
	mediaURLRef, err := url.Parse(mediaURI)
	if err != nil {
		t.Fatalf("RESULT=media_uri_invalid")
	}
	mediaURL := playlistBase.ResolveReference(mediaURLRef).String()

	// If the first reference is another playlist, fetch it and select its first
	// media URI. This handles common master-playlist layouts.
	for depth := 0; depth < 2; depth++ {
		ref, _ := url.Parse(mediaURL)
		if ref == nil || !strings.Contains(strings.ToLower(ref.Path), ".m3u8") {
			break
		}
		nestedResp, nestedBody, nestedErr := probeGet(client, mediaURL, "")
		if nestedErr != nil {
			t.Fatalf("RESULT=nested_playlist_request_failed error_type=%T", nestedErr)
		}
		if nestedResp.StatusCode < 200 || nestedResp.StatusCode >= 300 || !isM3U8(nestedBody) {
			t.Fatalf("RESULT=nested_playlist_invalid status=%d m3u8_signature=%t",
				nestedResp.StatusCode, isM3U8(nestedBody))
		}
		nestedBase := mediaURL
		if nestedResp.Request != nil && nestedResp.Request.URL != nil {
			nestedBase = nestedResp.Request.URL.String()
		}
		nextURI, nextErr := firstPlaylistURI(nestedBody)
		if nextErr != nil {
			t.Fatalf("RESULT=nested_playlist_no_media_uri")
		}
		baseURL, _ := url.Parse(nestedBase)
		nextURL, _ := url.Parse(nextURI)
		mediaURL = baseURL.ResolveReference(nextURL).String()
	}

	segmentResp, segmentBody, err := probeGet(client, mediaURL, "bytes=0-1023")
	if err != nil {
		t.Fatalf("RESULT=segment_request_failed error_type=%T", err)
	}
	segmentType := strings.ToLower(segmentResp.Header.Get("Content-Type"))
	if segmentResp.StatusCode != http.StatusOK && segmentResp.StatusCode != http.StatusPartialContent {
		t.Fatalf("RESULT=segment_http_failed status=%d content_type=%q", segmentResp.StatusCode, segmentType)
	}
	if len(segmentBody) == 0 || strings.Contains(segmentType, "text/html") || isM3U8(segmentBody) {
		t.Fatalf("RESULT=segment_body_invalid status=%d content_type=%q body_bytes=%d",
			segmentResp.StatusCode, segmentType, len(segmentBody))
	}
	segmentHost := ""
	if segmentResp.Request != nil && segmentResp.Request.URL != nil {
		segmentHost = segmentResp.Request.URL.Host
	}
	t.Logf("RESULT=segment_ok status=%d content_type=%q host=%s sampled_bytes=%d elapsed_ms=%d",
		segmentResp.StatusCode, segmentType, segmentHost, len(segmentBody),
		time.Since(started).Milliseconds())

	// Recheck the original URL at fixed offsets from its first resolution.
	for _, offset := range []time.Duration{1 * time.Minute, 5 * time.Minute} {
		wait := time.Until(issuedAt.Add(offset))
		if wait > 0 {
			t.Logf("WAITING_FOR_URL_AGE=%s", offset)
			time.Sleep(wait)
		}
		checkResp, checkBody, checkErr := probeGet(client, streamURL, "")
		if checkErr != nil {
			t.Logf("RESULT=url_age_check age=%s request_error_type=%T", offset, checkErr)
			continue
		}
		ageOK := checkResp.StatusCode >= 200 && checkResp.StatusCode < 300 && isM3U8(checkBody)
		t.Logf("RESULT=url_age_check age=%s status=%d m3u8_signature=%t body_bytes=%d valid=%t",
			offset, checkResp.StatusCode, isM3U8(checkBody), len(checkBody), ageOK)
	}

	t.Logf("RESULT=PROBE_COMPLETE total_elapsed=%s", time.Since(started).Round(time.Second))
}
