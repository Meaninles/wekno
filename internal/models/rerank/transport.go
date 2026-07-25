package rerank

import (
	"net/http"
	"time"

	secutils "github.com/Tencent/WeKnora/internal/utils"
)

func newRerankHTTPClient(timeout time.Duration) *http.Client {
	config := secutils.DefaultSSRFSafeHTTPClientConfig()
	config.Timeout = timeout
	return secutils.NewSSRFSafeHTTPClient(config)
}
