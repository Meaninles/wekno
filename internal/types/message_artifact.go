package types

type MessageArtifact struct {
	FileToken   string `json:"file_token"`
	FileName    string `json:"filename"`
	FileType    string `json:"file_type"`
	FileSize    int64  `json:"file_size"`
	SHA256      string `json:"sha256"`
	ContentType string `json:"content_type"`
	ArtifactID  string `json:"artifact_id,omitempty"`
	DownloadURL string `json:"download_url,omitempty"`
	Persisted   bool   `json:"persisted,omitempty"`
}
