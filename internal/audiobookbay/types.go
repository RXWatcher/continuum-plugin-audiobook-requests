package audiobookbay

type Config struct {
	BaseURL      string
	QBitURL      string
	QBitUsername string
	QBitPassword string
	Category     string
	SavePath     string
	Trackers     []string
	SearchLimit  int
}

type SearchHit struct {
	SourceID  string   `json:"source_id"`
	Source    string   `json:"source"`
	Title     string   `json:"title"`
	Authors   []string `json:"authors,omitempty"`
	DetailURL string   `json:"detail_url"`
	InfoHash  string   `json:"info_hash,omitempty"`
	MagnetURI string   `json:"magnet_uri,omitempty"`
	Size      string   `json:"size,omitempty"`
	Seeders   int      `json:"seeders,omitempty"`
	Leechers  int      `json:"leechers,omitempty"`
}

type DownloadResponse struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Magnet   string `json:"magnet,omitempty"`
	Title    string `json:"title,omitempty"`
	Progress int    `json:"progress,omitempty"`
}
