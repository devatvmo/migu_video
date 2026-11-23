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

// summarizeResolverError exposes only JSON field names and the request ID.
// It never prints the response body or the temporary playback URL.
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

// TestCCTV1Probe runs only against the public/anonymous HD stream.
// It deliberately never prints or stores the temporary playback URL.
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
		t.Fatalf("RESULT=resolve_invalid_url elapsed_ms=%d",
			time.Since(started).Milliseconds())
	}

	t.Logf("RESULT=resolve_ok scheme=%s host=%s elapsed_ms=%d",
		parsed.Scheme, parsed.Host, time.Since(started).Milliseconds())

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(http.MethodGet, streamURL, nil)
	if err != nil {
		t.Fatalf("RESULT=playlist_request_build_failed error_type=%T", err)
	}
	req.Header.Set("User-Agent", "okhttp/4.9.3")
	req.Header.Set("Referer", "https://m.miguvideo.com/")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("RESULT=playlist_request_failed error_type=%T", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		t.Fatalf("RESULT=playlist_read_failed error_type=%T", err)
	}

	isM3U8 := bytes.Contains(body, []byte("#EXTM3U"))
	finalHost := ""
	if resp.Request != nil && resp.Request.URL != nil {
		finalHost = resp.Request.URL.Host
	}

	t.Logf("RESULT=playlist_checked status=%d content_type=%q final_host=%s m3u8_signature=%t body_bytes=%d elapsed_ms=%d",
		resp.StatusCode, resp.Header.Get("Content-Type"), finalHost, isM3U8,
		len(body), time.Since(started).Milliseconds())

	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !isM3U8 {
		t.Fatalf("RESULT=playlist_not_verified")
	}
	t.Log("RESULT=PASS")
}
