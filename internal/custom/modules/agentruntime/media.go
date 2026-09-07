package agentruntime

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	filesvc "github.com/Tencent/WeKnora/internal/application/service/file"
	"github.com/Tencent/WeKnora/internal/types"
)

// Stored images are read with the tenant's storage credentials and sent as
// inline images. Providers never need access to internal storage hostnames.
func (s *Service) prepareRuntimeMedia(ctx context.Context, payload *ChatPayload) error {
	cache := map[string]string{}
	inline := func(raw string) (string, error) {
		if cached, ok := cache[raw]; ok {
			return cached, nil
		}
		u, err := url.Parse(raw)
		if err != nil {
			return "", err
		}
		if raw == "" || u.Scheme == "data" || u.Scheme == "http" || u.Scheme == "https" {
			return raw, nil
		}
		storage := s.fileService
		tenant, _ := ctx.Value(types.TenantInfoContextKey).(*types.Tenant)
		if tenant != nil && tenant.StorageEngineConfig != nil {
			storage, _, err = filesvc.NewFileServiceFromStorageConfig(u.Scheme, tenant.StorageEngineConfig, "")
			if err != nil {
				return "", err
			}
		}
		if storage == nil {
			return "", fmt.Errorf("image storage unavailable")
		}
		reader, err := storage.GetFile(ctx, raw)
		if err != nil {
			return "", err
		}
		defer reader.Close()
		data, err := io.ReadAll(io.LimitReader(reader, (10<<20)+1))
		if err != nil {
			return "", err
		}
		media := http.DetectContentType(data)
		if len(data) > 10<<20 || !strings.HasPrefix(media, "image/") {
			return "", fmt.Errorf("invalid stored image")
		}
		cache[raw] = "data:" + media + ";base64," + base64.StdEncoding.EncodeToString(data)
		return cache[raw], nil
	}
	for i := range payload.History {
		for j := range payload.History[i].Images {
			image := &payload.History[i].Images[j]
			var err error
			image.URL, err = inline(image.URL)
			if err != nil {
				return fmt.Errorf("restore conversation image: %w", err)
			}
		}
	}
	for i, raw := range payload.ImageURLs {
		image, err := inline(raw)
		if err != nil {
			return fmt.Errorf("prepare current image: %w", err)
		}
		payload.ImageURLs[i] = image
	}
	return nil
}
