package documentqueue

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKubernetesInstanceIdentityRoundTrip(t *testing.T) {
	instanceID, err := formatKubernetesInstanceID("tenant-system", "7de43a20-uid")
	require.NoError(t, err)
	require.Equal(t, "k8s/tenant-system/7de43a20-uid", instanceID)

	identity, ok := parseKubernetesInstanceID(instanceID)
	require.True(t, ok)
	require.Equal(t, "tenant-system", identity.Namespace)
	require.Equal(t, "7de43a20-uid", identity.UID)

	for _, invalid := range []string{
		"tenant-system/uid",
		"k8s/tenant-system",
		"k8s/tenant-system/pod/uid/extra",
		"k8s/tenant-system/uid\nforged",
	} {
		_, parsed := parseKubernetesInstanceID(invalid)
		require.False(t, parsed, invalid)
	}
}

func TestKubernetesRuntimeVerifierRequiresPositiveExactTermination(t *testing.T) {
	const expectedPath = "/api/v1/namespaces/runtime-ns/pods"
	tests := []struct {
		name       string
		statusCode int
		body       string
		proven     bool
		reason     string
		wantErr    bool
	}{
		{
			name:       "current app container terminated",
			statusCode: http.StatusOK,
			body: `{"metadata":{"name":"parser-pod","uid":"pod-uid"},"status":{"phase":"Running","containerStatuses":[` +
				`{"name":"app","state":{"terminated":{"exitCode":137,"reason":"Error","finishedAt":"2026-07-22T10:00:00Z"}}}]}}`,
			proven: true,
			reason: "app_container_terminated",
		},
		{
			name:       "pod terminal",
			statusCode: http.StatusOK,
			body:       `{"metadata":{"name":"parser-pod","uid":"pod-uid"},"status":{"phase":"Failed"}}`,
			proven:     true,
			reason:     "pod_terminal",
		},
		{
			name:       "deletion timestamp is only intent",
			statusCode: http.StatusOK,
			body: `{"metadata":{"name":"parser-pod","uid":"pod-uid","deletionTimestamp":"2026-07-22T10:00:00Z"},` +
				`"status":{"phase":"Running","containerStatuses":[{"name":"app","state":{"running":{}}}]}}`,
			reason: "runtime_still_not_proven_terminated",
		},
		{
			name:       "last terminated state with current running is not proof",
			statusCode: http.StatusOK,
			body: `{"metadata":{"name":"parser-pod","uid":"pod-uid"},"status":{"phase":"Running","containerStatuses":[` +
				`{"name":"app","state":{"running":{}},"lastState":{"terminated":{"exitCode":1}}}]}}`,
			reason: "runtime_still_not_proven_terminated",
		},
		{
			name:       "different container termination is not proof",
			statusCode: http.StatusOK,
			body: `{"metadata":{"name":"parser-pod","uid":"pod-uid"},"status":{"phase":"Running","containerStatuses":[` +
				`{"name":"metrics","state":{"terminated":{"exitCode":0}}},` +
				`{"name":"app","state":{"running":{}}}]}}`,
			reason: "runtime_still_not_proven_terminated",
		},
		{
			name:       "target UID absent is not proof",
			statusCode: http.StatusOK,
			body:       `{"metadata":{"name":"replacement-pod","uid":"replacement-uid"},"status":{"phase":"Failed"}}`,
			reason:     "pod_uid_not_present_is_not_proof",
		},
		{
			name:       "404 is not proof",
			statusCode: http.StatusNotFound,
			body:       `{"reason":"NotFound"}`,
			reason:     "pod_uid_not_present_is_not_proof",
		},
		{
			name:       "API failure fails closed",
			statusCode: http.StatusServiceUnavailable,
			body:       `{"message":"unavailable"}`,
			wantErr:    true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				require.Equal(t, expectedPath, r.URL.Path)
				require.Equal(t, "100", r.URL.Query().Get("limit"))
				require.Equal(t, "Bearer rotating-token", r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.statusCode)
				if test.statusCode == http.StatusOK {
					_, _ = fmt.Fprintf(w, `{"metadata":{},"items":[%s]}`, test.body)
				} else {
					_, _ = fmt.Fprint(w, test.body)
				}
			}))
			defer server.Close()
			baseURL, err := url.Parse(server.URL)
			require.NoError(t, err)
			verifier := newKubernetesRuntimeVerifierWithClient(
				baseURL,
				server.Client(),
				"app",
				func() (string, error) { return "rotating-token", nil },
			)

			evidence, err := verifier.VerifyTermination(
				context.Background(), "k8s/runtime-ns/pod-uid", "boot-old",
			)
			if test.wantErr {
				require.Error(t, err)
				require.False(t, evidence.Proven)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.proven, evidence.Proven)
			require.Equal(t, test.reason, evidence.Reason)
			require.EqualValues(t, 1, requests.Load())
		})
	}
}

func TestKubernetesRuntimeVerifierRejectsUnsupportedIdentityWithoutCallingAPI(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	baseURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	verifier := newKubernetesRuntimeVerifierWithClient(
		baseURL, server.Client(), "app", func() (string, error) { return "token", nil },
	)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	evidence, err := verifier.VerifyTermination(ctx, "docker/instance", "boot-old")
	require.NoError(t, err)
	require.False(t, evidence.Proven)
	require.Equal(t, "unsupported_instance_identity", evidence.Reason)
	require.Zero(t, requests.Load())
}

func TestKubernetesRuntimeVerifierFindsExactUIDAcrossListPages(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 1:
			require.Empty(t, r.URL.Query().Get("continue"))
			_, _ = fmt.Fprint(w, `{"metadata":{"continue":"next+page/token"},"items":[`+
				`{"metadata":{"name":"replacement","uid":"new-uid"},"status":{"phase":"Failed"}}]}`)
		case 2:
			require.Equal(t, "next+page/token", r.URL.Query().Get("continue"))
			_, _ = fmt.Fprint(w, `{"metadata":{},"items":[`+
				`{"metadata":{"name":"old-parser","uid":"old-uid"},"status":{"phase":"Succeeded"}}]}`)
		default:
			t.Fatalf("unexpected Kubernetes list page %d", call)
		}
	}))
	defer server.Close()
	baseURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	verifier := newKubernetesRuntimeVerifierWithClient(
		baseURL, server.Client(), "app", func() (string, error) { return "token", nil },
	)

	evidence, err := verifier.VerifyTermination(
		context.Background(), "k8s/runtime-ns/old-uid", "boot-old",
	)
	require.NoError(t, err)
	require.True(t, evidence.Proven)
	require.Equal(t, "pod_terminal", evidence.Reason)
	require.EqualValues(t, 2, requests.Load())
}
