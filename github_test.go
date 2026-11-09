package main

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestParseRateLimit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tests := []struct {
		name    string
		status  int
		headers map[string]string
		body    string
		want    time.Duration
	}{
		{"ok status", 404, nil, "", 0},
		{"plain 403", 403, map[string]string{"X-RateLimit-Remaining": "12"}, `{"message":"Resource not accessible"}`, 0},
		{"primary limit", 403, map[string]string{
			"X-RateLimit-Remaining": "0",
			"X-RateLimit-Reset":     strconv.FormatInt(now.Add(10*time.Minute).Unix(), 10),
		}, `{"message":"API rate limit exceeded for 1.2.3.4."}`, 10*time.Minute + 2*time.Second},
		{"reset in the past", 403, map[string]string{
			"X-RateLimit-Remaining": "0",
			"X-RateLimit-Reset":     strconv.FormatInt(now.Add(-time.Minute).Unix(), 10),
		}, "", time.Second},
		{"retry-after", 403, map[string]string{"Retry-After": "30"}, "", 30 * time.Second},
		{"429 without headers", 429, nil, "", defaultRateLimitWait},
		{"secondary limit body only", 403, nil, `{"message":"You have exceeded a secondary rate limit."}`, defaultRateLimitWait},
		{"capped", 429, map[string]string{"Retry-After": "86400"}, "", maxRateLimitWait},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tt.status, Header: http.Header{}}
			for k, v := range tt.headers {
				resp.Header.Set(k, v)
			}
			if got := parseRateLimit(resp, tt.body, now); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
