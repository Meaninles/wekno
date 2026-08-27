package langfuse

import (
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/agenteval"
)

func TestOTLPHTTPExporterUsesV4EndpointAndNeverBlocksBusiness(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/public/otel/v1/traces" {
			t.Errorf("path=%s", request.URL.Path)
		}
		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("pk:sk"))
		if request.Header.Get("Authorization") != wantAuth || request.Header.Get("x-langfuse-ingestion-version") != "4" {
			t.Errorf("unexpected headers: %#v", request.Header)
		}
		var reader io.Reader = request.Body
		if request.Header.Get("Content-Encoding") == "gzip" {
			gzipReader, err := gzip.NewReader(request.Body)
			if err != nil {
				t.Errorf("gzip: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			defer gzipReader.Close()
			reader = gzipReader
		}
		body, _ := io.ReadAll(reader)
		if len(body) == 0 {
			t.Error("empty OTLP body")
		}
		mu.Lock()
		requests++
		mu.Unlock()
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	cfg := testConfig(true)
	cfg.Host = server.URL
	cfg.PublicKey = "pk"
	cfg.SecretKey = "sk"
	manager, err := Init(cfg)
	if err != nil || !manager.Enabled() {
		t.Fatalf("init err=%v manager=%#v", err, manager)
	}
	_, root := manager.StartTrace(context.Background(), TraceOptions{Name: "test"})
	root.Finish("ok", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests == 0 {
		t.Fatal("no OTLP request received")
	}
}

func TestInvalidRecorderConfigFailsOpen(t *testing.T) {
	manager, err := Init(Config{
		Enabled: true, SampleRate: 1, QueueSize: 1, FlushAt: 2,
		AgentEval: agenteval.Config{Mode: agenteval.ModeEval, CapturePolicy: agenteval.CaptureFull},
	})
	if err != nil || manager.Enabled() {
		t.Fatalf("invalid recorder must return disabled manager without app error: manager=%#v err=%v", manager, err)
	}
}
