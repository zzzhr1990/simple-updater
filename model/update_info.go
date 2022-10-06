package model

type UpdateCollection struct {
	Channel  string        `json:"channel"`
	Platform string        `json:"platform"`
	Version  string        `json:"version"`
	Main     *UpdateInfo   `json:"main"`
	List     []*UpdateInfo `json:"list"`
	Type     string        `json:"type"`
}

type UpdateInfo struct {
	Path    string `json:"path"`
	SHA1    string `json:"sha1"`
	Size    int64  `json:"size"`
	Version string `json:"version"`
	URL     string `json:"url"`
	// Type    string `json:"type"`
}

type VersionInfo struct {
	Version string `json:"version"`
	SHA1    string `json:"sha1"`
}
