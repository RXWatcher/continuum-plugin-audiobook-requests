package server

import (
	"encoding/json"
	"net/http"
)

type capabilitiesResponse struct {
	Formats               []string `json:"formats"`
	Features              []string `json:"features"`
	SupportsRangeRequests bool     `json:"supports_range_requests"`
}

func (s *Server) handleCapabilities(w http.ResponseWriter, _ *http.Request) {
	resp := capabilitiesResponse{
		Formats:               []string{"m4b", "mp3", "m4a", "aac", "flac", "ogg", "opus"},
		Features:              []string{"request_provider", "external_search", "request_snapshot", "admin_diagnostics", "provider_test_search", "qbittorrent_enqueue"},
		SupportsRangeRequests: false,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
