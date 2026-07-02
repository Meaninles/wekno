package embedding

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEmbeddingTextMetricsDoNotContainRawText(t *testing.T) {
	secret := "客户合同编号 SECRET-EMBED-123456，金额 999999"

	single, err := json.Marshal(embeddingTextMetric(secret))
	if err != nil {
		t.Fatalf("marshal single metric: %v", err)
	}
	if strings.Contains(string(single), "SECRET-EMBED-123456") || strings.Contains(string(single), "客户合同编号") {
		t.Fatalf("single metric leaked raw embedding text: %s", string(single))
	}
	if !strings.Contains(string(single), "text_sha256") || !strings.Contains(string(single), "text_runes") {
		t.Fatalf("single metric missing digest/length fields: %s", string(single))
	}

	batch, err := json.Marshal(embeddingTextMetrics([]string{secret, "另一段敏感文本 SECRET-EMBED-2"}))
	if err != nil {
		t.Fatalf("marshal batch metrics: %v", err)
	}
	if strings.Contains(string(batch), "SECRET-EMBED") || strings.Contains(string(batch), "敏感文本") {
		t.Fatalf("batch metrics leaked raw embedding text: %s", string(batch))
	}
	if !strings.Contains(string(batch), "\"index\":0") || !strings.Contains(string(batch), "\"index\":1") {
		t.Fatalf("batch metrics missing stable indexes: %s", string(batch))
	}
}
