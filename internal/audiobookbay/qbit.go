package audiobookbay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
)

type QBitClient struct {
	baseURL  string
	username string
	password string
	hc       *http.Client
}

type TorrentInfo struct {
	Hash     string  `json:"hash"`
	Name     string  `json:"name"`
	State    string  `json:"state"`
	Progress float64 `json:"progress"`
}

func NewQBitClient(baseURL, username, password string) *QBitClient {
	jar, _ := cookiejar.New(nil)
	return &QBitClient{
		baseURL:  strings.TrimRight(baseURL, "/"),
		username: username,
		password: password,
		hc:       &http.Client{Timeout: defaultTimeout, Jar: jar},
	}
}

func (c *QBitClient) Login(ctx context.Context) error {
	if c.baseURL == "" {
		return fmt.Errorf("qbittorrent_url is required")
	}
	form := url.Values{}
	form.Set("username", c.username)
	form.Set("password", c.password)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v2/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode >= 400 || !strings.Contains(string(body), "Ok.") {
		return fmt.Errorf("qbittorrent login failed: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (c *QBitClient) AddMagnet(ctx context.Context, magnet, category, savePath string) error {
	if err := c.Login(ctx); err != nil {
		return err
	}
	form := url.Values{}
	form.Set("urls", magnet)
	if category != "" {
		form.Set("category", category)
	}
	if savePath != "" {
		form.Set("savepath", savePath)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v2/torrents/add", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("qbittorrent add failed: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (c *QBitClient) Torrent(ctx context.Context, hash string) (TorrentInfo, error) {
	if err := c.Login(ctx); err != nil {
		return TorrentInfo{}, err
	}
	reqURL := c.baseURL + "/api/v2/torrents/info?hashes=" + url.QueryEscape(hash)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return TorrentInfo{}, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return TorrentInfo{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return TorrentInfo{}, err
	}
	if resp.StatusCode >= 400 {
		return TorrentInfo{}, fmt.Errorf("qbittorrent info failed: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var items []TorrentInfo
	if err := json.Unmarshal(body, &items); err != nil {
		return TorrentInfo{}, err
	}
	if len(items) == 0 {
		return TorrentInfo{}, nil
	}
	return items[0], nil
}
