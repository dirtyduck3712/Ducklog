// Package vl 是 VictoriaLogs LogsQL 的薄 HTTP client(唯讀查詢)。
package vl

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	base string
	hc   *http.Client
	user string
	pass string
}

func New(baseURL string, timeout time.Duration) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), hc: &http.Client{Timeout: timeout}}
}

// NewWithAuth 同 New,但每個請求附 HTTP Basic Auth(VL 開了 -httpAuth.* 時用)。
func NewWithAuth(baseURL string, timeout time.Duration, user, pass string) *Client {
	c := New(baseURL, timeout)
	c.user, c.pass = user, pass
	return c
}

// auth 在有設憑證時為 request 附上 Basic Auth。
func (c *Client) auth(req *http.Request) {
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
}

func (c *Client) Ping(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/health", nil)
	c.auth(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("VL health %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) Query(ctx context.Context, logsql string, limit int) ([]map[string]any, error) {
	v := url.Values{"query": {logsql}}
	if limit > 0 {
		v.Set("limit", strconv.Itoa(limit))
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/select/logsql/query?"+v.Encode(), nil)
	c.auth(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("VL query %d: %s", resp.StatusCode, readErr(resp))
	}
	var out []map[string]any
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("parse VL row: %w", err)
		}
		out = append(out, m)
	}
	return out, sc.Err()
}

func (c *Client) Count(ctx context.Context, logsql string) (int64, error) {
	rows, err := c.Query(ctx, logsql+" | stats count() as n", 0)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	// VL 回字串數字
	switch v := rows[0]["n"].(type) {
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n, nil
	case float64:
		return int64(v), nil
	}
	return 0, nil
}

func readErr(resp *http.Response) string {
	b := make([]byte, 512)
	n, _ := resp.Body.Read(b)
	return string(b[:n])
}
