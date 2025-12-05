package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	httpClient *http.Client
	allowHTTP  bool
	userAgent  string
}

func NewClient(timeout time.Duration, allowInsecureHTTP bool, userAgent string) *Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
	}
	return &Client{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   timeout,
		},
		allowHTTP: allowInsecureHTTP,
		userAgent: userAgent,
	}
}

func (c *Client) Do(ctx context.Context, method, url string, body io.Reader, headers http.Header) (*http.Response, error) {
	if !c.allowHTTP && urlHasInsecureScheme(url) {
		return nil, errors.New("http upstream not allowed; set ALLOW_INSECURE_HTTP to permit")
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	for k, vals := range headers {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream request failed: %w", err)
	}
	return resp, nil
}

func urlHasInsecureScheme(u string) bool {
	return len(u) >= 7 && u[:7] == "http://"
}
