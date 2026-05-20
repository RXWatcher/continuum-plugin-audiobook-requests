// Package nzbget is a minimal JSON-RPC client for NZBGet, covering exactly
// the surface this plugin needs: append a URL with an optional unpack
// password, and look up a queued/historical job to map its state into the
// plugin's queue states.
//
// Reference docs: https://nzbget.com/documentation/api/
package nzbget

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultTimeout  = 15 * time.Second
	maxResponseSize = 4 << 20
)

// Client wraps an NZBGet JSON-RPC endpoint with credentials baked into the
// request URL (NZBGet's recommended approach for HTTP basic auth on the
// JSON-RPC channel).
type Client struct {
	endpoint string // includes user:pass@ when configured
	hc       *http.Client
}

// New builds a Client. baseURL is the NZBGet web URL root, e.g.
// http://nzbget.lan:6789 — no trailing slash, no /jsonrpc path; this
// constructor appends them. user / pass are optional (NZBGet behind a
// trust boundary can omit them); when present they're embedded in the
// request URL via url.UserPassword.
func New(baseURL, user, pass string) (*Client, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("nzbget: base url is required")
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("nzbget: parse base url: %w", err)
	}
	if user != "" {
		u.User = url.UserPassword(user, pass)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/jsonrpc"
	return &Client{
		endpoint: u.String(),
		hc:       &http.Client{Timeout: defaultTimeout},
	}, nil
}

type rpcRequest struct {
	Method string `json:"method"`
	Params []any  `json:"params"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *Client) call(ctx context.Context, method string, params []any, out any) error {
	body, err := json.Marshal(rpcRequest{Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("nzbget marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("nzbget %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return fmt.Errorf("nzbget read body: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("nzbget %s: unauthorized (check NZBGet user/password)", method)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("nzbget %s: HTTP %d: %s", method, resp.StatusCode, truncForError(raw))
	}
	var env rpcResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("nzbget %s: decode: %w", method, err)
	}
	if env.Error != nil {
		return fmt.Errorf("nzbget %s: rpc error %d: %s", method, env.Error.Code, env.Error.Message)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("nzbget %s: decode result: %w", method, err)
	}
	return nil
}

// Append submits an NZB by URL into NZBGet's queue. Returns the assigned
// NZBID (NZBGet's per-job integer identifier) for later polling.
//
// PPParameters is used to set the unpack password — NZBGet's official
// recipe for "unrar with a specific password" without intervening at
// extract time.
func (c *Client) Append(ctx context.Context, opts AppendOptions) (int, error) {
	if opts.URL == "" {
		return 0, fmt.Errorf("nzbget append: URL is required")
	}
	if opts.Name == "" {
		opts.Name = "audiobook-request"
	}
	pp := []map[string]string{}
	if opts.UnpackPassword != "" {
		pp = append(pp, map[string]string{
			"Name":  "*Unpack:Password",
			"Value": opts.UnpackPassword,
		})
	}
	params := []any{
		opts.Name + ".nzb", // NZBFilename
		opts.URL,           // Content (URL form)
		opts.Category,      // Category
		0,                  // Priority (normal)
		false,              // AddToTop
		false,              // AddPaused
		"",                 // DupeKey
		0,                  // DupeScore
		"SCORE",            // DupeMode
		pp,                 // PPParameters
	}
	var nzbID int
	if err := c.call(ctx, "append", params, &nzbID); err != nil {
		return 0, err
	}
	if nzbID <= 0 {
		return 0, fmt.Errorf("nzbget append: server returned id %d (likely DupeMode rejection)", nzbID)
	}
	return nzbID, nil
}

type AppendOptions struct {
	URL            string
	Name           string
	Category       string
	UnpackPassword string
}

// Status maps the NZBGet job state into the plugin's queue vocabulary.
// "downloading" covers both literal downloading and post-processing (the
// download isn't done from the user's POV until extraction finishes).
type Status struct {
	NZBID    int    `json:"nzbId"`
	State    string `json:"state"` // queued | downloading | imported | failed | unknown
	Progress int    `json:"progress,omitempty"`
	Error    string `json:"error,omitempty"`
}

// listGroupsEntry is the subset of listgroups response fields we care
// about. Real response has ~40 fields per group; we only need state +
// progress.
type listGroupsEntry struct {
	NZBID            int    `json:"NZBID"`
	NZBName          string `json:"NZBName"`
	Status           string `json:"Status"`
	FileSizeMB       int64  `json:"FileSizeMB"`
	RemainingSizeMB  int64  `json:"RemainingSizeMB"`
	DownloadedSizeMB int64  `json:"DownloadedSizeMB"`
}

// historyEntry covers the fields needed to distinguish SUCCESS vs FAILURE
// vs DELETED for completed jobs.
type historyEntry struct {
	NZBID        int    `json:"NZBID"`
	NZBName      string `json:"NZBName"`
	Status       string `json:"Status"` // e.g. SUCCESS/ALL, FAILURE/PAR, DELETED/MANUAL, ...
	DeleteStatus string `json:"DeleteStatus"`
	ParStatus    string `json:"ParStatus"`
	UnpackStatus string `json:"UnpackStatus"`
}

// Lookup resolves an NZBID to its current state by checking the live
// queue (listgroups) first, falling back to history. If the id is in
// neither, returns ("unknown", nil) so the reconciler can decide whether
// to treat it as missing.
func (c *Client) Lookup(ctx context.Context, nzbID int) (Status, error) {
	var queue []listGroupsEntry
	if err := c.call(ctx, "listgroups", []any{0}, &queue); err != nil {
		return Status{}, err
	}
	for _, g := range queue {
		if g.NZBID == nzbID {
			s := Status{NZBID: nzbID, State: mapQueueStatus(g.Status)}
			if g.FileSizeMB > 0 {
				s.Progress = int(g.DownloadedSizeMB * 100 / g.FileSizeMB)
			}
			return s, nil
		}
	}
	// Not in active queue → check history. NZBGet history retains
	// completed/failed jobs.
	var hist []historyEntry
	if err := c.call(ctx, "history", []any{false}, &hist); err != nil {
		return Status{}, err
	}
	for _, h := range hist {
		if h.NZBID == nzbID {
			return Status{NZBID: nzbID, State: mapHistoryStatus(h.Status), Progress: 100}, nil
		}
	}
	return Status{NZBID: nzbID, State: "unknown"}, nil
}

// Version probes the daemon to confirm reachability + auth. Used by the
// admin "Test connection" button.
func (c *Client) Version(ctx context.Context) (string, error) {
	var v string
	if err := c.call(ctx, "version", nil, &v); err != nil {
		return "", err
	}
	return v, nil
}

// mapQueueStatus translates NZBGet's live-queue status strings into the
// plugin's queue vocabulary. Source values come from the NZBGet API docs
// (DOWNLOADING, PAUSED, QUEUED, FETCHING, PP_QUEUED, ...).
func mapQueueStatus(s string) string {
	switch strings.ToUpper(s) {
	case "QUEUED", "FETCHING":
		return "queued"
	case "DOWNLOADING",
		"PAUSED",
		"PP_QUEUED",
		"LOADING_PARS",
		"VERIFYING_SOURCES",
		"REPAIRING",
		"VERIFYING_REPAIRED",
		"UNPACKING",
		"MOVING",
		"EXECUTING_SCRIPT":
		return "downloading"
	}
	return "downloading"
}

// mapHistoryStatus translates NZBGet's history status strings. SUCCESS/*
// means everything completed cleanly (downloaded + verified + unpacked);
// anything else is a failure we surface to the operator.
func mapHistoryStatus(s string) string {
	if strings.HasPrefix(strings.ToUpper(s), "SUCCESS") {
		return "imported"
	}
	return "failed"
}

const errBodySnippet = 256

func truncForError(b []byte) string {
	if len(b) <= errBodySnippet {
		return string(b)
	}
	return string(b[:errBodySnippet]) + "…(truncated)"
}
