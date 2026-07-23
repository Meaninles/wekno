package documentqueue

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	kubernetesInstancePrefix     = "k8s/"
	maxKubernetesInstanceIDBytes = 160
	maxKubernetesResponseBytes   = 4 << 20
	kubernetesPodListPageSize    = 100
)

var (
	kubernetesNamespacePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	kubernetesUIDPattern       = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]*[A-Za-z0-9])?$`)
)

// RuntimeTerminationEvidence is deliberately positive-only. Callers may use
// Proven=true as a fencing fact; every other result, including an absent Pod,
// is merely an inability to prove termination.
type RuntimeTerminationEvidence struct {
	Proven bool
	Proof  string
	Reason string
}

// RuntimeTerminationVerifier proves that the exact runtime identity which
// owned a workflow can no longer execute. Implementations must fail closed:
// heartbeat age, API absence and object deletion intent are not proof.
type RuntimeTerminationVerifier interface {
	VerifyTermination(context.Context, string, string) (RuntimeTerminationEvidence, error)
}

type kubernetesPodIdentity struct {
	Namespace string
	UID       string
}

func formatKubernetesInstanceID(namespace, uid string) (string, error) {
	identity := kubernetesPodIdentity{
		Namespace: strings.TrimSpace(namespace),
		UID:       strings.TrimSpace(uid),
	}
	if err := validateKubernetesPodIdentity(identity); err != nil {
		return "", err
	}
	instanceID := kubernetesInstancePrefix + identity.Namespace + "/" + identity.UID
	if len(instanceID) > maxKubernetesInstanceIDBytes {
		return "", fmt.Errorf("kubernetes document-queue instance identity exceeds %d bytes", maxKubernetesInstanceIDBytes)
	}
	return instanceID, nil
}

func parseKubernetesInstanceID(instanceID string) (kubernetesPodIdentity, bool) {
	instanceID = strings.TrimSpace(instanceID)
	if !strings.HasPrefix(instanceID, kubernetesInstancePrefix) || len(instanceID) > maxKubernetesInstanceIDBytes {
		return kubernetesPodIdentity{}, false
	}
	parts := strings.Split(strings.TrimPrefix(instanceID, kubernetesInstancePrefix), "/")
	if len(parts) != 2 {
		return kubernetesPodIdentity{}, false
	}
	identity := kubernetesPodIdentity{Namespace: parts[0], UID: parts[1]}
	if validateKubernetesPodIdentity(identity) != nil {
		return kubernetesPodIdentity{}, false
	}
	return identity, true
}

func validateKubernetesPodIdentity(identity kubernetesPodIdentity) error {
	if len(identity.Namespace) > 63 || !kubernetesNamespacePattern.MatchString(identity.Namespace) {
		return errors.New("kubernetes namespace is invalid")
	}
	if len(identity.UID) > 128 || !kubernetesUIDPattern.MatchString(identity.UID) {
		return errors.New("kubernetes pod UID is invalid")
	}
	return nil
}

type kubernetesRuntimeVerifier struct {
	apiServer    *url.URL
	client       *http.Client
	container    string
	tokenSource  func() (string, error)
	requestLimit int64
}

func newKubernetesRuntimeVerifier(config Config) (RuntimeTerminationVerifier, error) {
	if !config.KubernetesRuntimeVerifierEnabled {
		return nil, nil
	}
	apiServer, err := url.Parse(strings.TrimSpace(config.KubernetesAPIServer))
	if err != nil || apiServer.Scheme != "https" || apiServer.Host == "" {
		return nil, errors.New("Kubernetes runtime verifier requires an https API server URL")
	}
	caPEM, err := os.ReadFile(strings.TrimSpace(config.KubernetesCAFile))
	if err != nil {
		return nil, fmt.Errorf("read Kubernetes API CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("Kubernetes API CA file contains no certificates")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	client := &http.Client{Transport: transport, Timeout: config.KubernetesRequestTimeout}
	tokenFile := strings.TrimSpace(config.KubernetesTokenFile)
	if tokenFile == "" {
		return nil, errors.New("Kubernetes runtime verifier token file is required")
	}
	return newKubernetesRuntimeVerifierWithClient(
		apiServer,
		client,
		config.KubernetesContainerName,
		func() (string, error) {
			token, readErr := os.ReadFile(tokenFile)
			if readErr != nil {
				return "", fmt.Errorf("read Kubernetes API token: %w", readErr)
			}
			value := strings.TrimSpace(string(token))
			if value == "" {
				return "", errors.New("Kubernetes API token is empty")
			}
			return value, nil
		},
	), nil
}

func newKubernetesRuntimeVerifierWithClient(
	apiServer *url.URL,
	client *http.Client,
	container string,
	tokenSource func() (string, error),
) *kubernetesRuntimeVerifier {
	return &kubernetesRuntimeVerifier{
		apiServer:    apiServer,
		client:       client,
		container:    strings.TrimSpace(container),
		tokenSource:  tokenSource,
		requestLimit: maxKubernetesResponseBytes,
	}
}

type kubernetesPodResponse struct {
	Metadata struct {
		Name              string     `json:"name"`
		UID               string     `json:"uid"`
		DeletionTimestamp *time.Time `json:"deletionTimestamp"`
	} `json:"metadata"`
	Status struct {
		Phase             string `json:"phase"`
		ContainerStatuses []struct {
			Name  string `json:"name"`
			State struct {
				Terminated *struct {
					ExitCode    int       `json:"exitCode"`
					Reason      string    `json:"reason"`
					FinishedAt  time.Time `json:"finishedAt"`
					ContainerID string    `json:"containerID"`
				} `json:"terminated"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

type kubernetesPodListResponse struct {
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	Items []kubernetesPodResponse `json:"items"`
}

func (v *kubernetesRuntimeVerifier) VerifyTermination(
	ctx context.Context,
	instanceID string,
	bootID string,
) (RuntimeTerminationEvidence, error) {
	identity, ok := parseKubernetesInstanceID(instanceID)
	if !ok {
		return RuntimeTerminationEvidence{Reason: "unsupported_instance_identity"}, nil
	}
	if strings.TrimSpace(bootID) == "" {
		return RuntimeTerminationEvidence{Reason: "missing_boot_identity"}, nil
	}
	if v == nil || v.apiServer == nil || v.client == nil || v.tokenSource == nil || v.container == "" {
		return RuntimeTerminationEvidence{}, errors.New("Kubernetes runtime verifier is not configured")
	}
	token, err := v.tokenSource()
	if err != nil {
		return RuntimeTerminationEvidence{}, err
	}
	continueToken := ""
	seenContinueTokens := make(map[string]struct{})
	for {
		endpoint := *v.apiServer
		endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/api/v1/namespaces/" +
			url.PathEscape(identity.Namespace) + "/pods"
		query := endpoint.Query()
		query.Set("limit", fmt.Sprintf("%d", kubernetesPodListPageSize))
		if continueToken != "" {
			query.Set("continue", continueToken)
		}
		endpoint.RawQuery = query.Encode()
		endpoint.Fragment = ""
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if requestErr != nil {
			return RuntimeTerminationEvidence{}, fmt.Errorf("build Kubernetes Pod verification request: %w", requestErr)
		}
		request.Header.Set("Accept", "application/json")
		request.Header.Set("Authorization", "Bearer "+token)
		response, requestErr := v.client.Do(request)
		if requestErr != nil {
			return RuntimeTerminationEvidence{}, fmt.Errorf("query Kubernetes Pod runtime state: %w", requestErr)
		}
		if response.StatusCode == http.StatusNotFound {
			_ = response.Body.Close()
			// Absence cannot distinguish a terminated Pod from an unreachable,
			// partitioned or prematurely garbage-collected runtime identity.
			return RuntimeTerminationEvidence{Reason: "pod_uid_not_present_is_not_proof"}, nil
		}
		if response.StatusCode != http.StatusOK {
			detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
			_ = response.Body.Close()
			return RuntimeTerminationEvidence{}, fmt.Errorf(
				"Kubernetes Pod verification returned %s: %s",
				response.Status,
				strings.TrimSpace(string(detail)),
			)
		}
		limit := v.requestLimit
		if limit <= 0 {
			limit = maxKubernetesResponseBytes
		}
		var list kubernetesPodListResponse
		decodeErr := json.NewDecoder(io.LimitReader(response.Body, limit)).Decode(&list)
		_ = response.Body.Close()
		if decodeErr != nil {
			return RuntimeTerminationEvidence{}, fmt.Errorf("decode Kubernetes Pod runtime state: %w", decodeErr)
		}
		for i := range list.Items {
			if strings.TrimSpace(list.Items[i].Metadata.UID) != identity.UID {
				continue
			}
			return v.terminationEvidenceForPod(identity, &list.Items[i]), nil
		}
		continueToken = strings.TrimSpace(list.Metadata.Continue)
		if continueToken == "" {
			return RuntimeTerminationEvidence{Reason: "pod_uid_not_present_is_not_proof"}, nil
		}
		if _, duplicated := seenContinueTokens[continueToken]; duplicated {
			return RuntimeTerminationEvidence{}, errors.New("Kubernetes Pod list repeated its continuation token")
		}
		seenContinueTokens[continueToken] = struct{}{}
	}
}

func (v *kubernetesRuntimeVerifier) terminationEvidenceForPod(
	identity kubernetesPodIdentity,
	pod *kubernetesPodResponse,
) RuntimeTerminationEvidence {
	if pod == nil || strings.TrimSpace(pod.Metadata.UID) != identity.UID {
		return RuntimeTerminationEvidence{Reason: "pod_uid_not_present_is_not_proof"}
	}
	podName := strings.TrimSpace(pod.Metadata.Name)
	phase := strings.TrimSpace(pod.Status.Phase)
	if phase == "Succeeded" || phase == "Failed" {
		return RuntimeTerminationEvidence{
			Proven: true,
			Proof: fmt.Sprintf(
				"kubernetes:pod_terminal:%s/%s:%s:phase=%s",
				identity.Namespace, podName, identity.UID, phase,
			),
			Reason: "pod_terminal",
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != v.container || status.State.Terminated == nil {
			continue
		}
		terminated := status.State.Terminated
		return RuntimeTerminationEvidence{
			Proven: true,
			Proof: fmt.Sprintf(
				"kubernetes:container_terminated:%s/%s:%s:container=%s:exit=%d:finished=%s",
				identity.Namespace,
				podName,
				identity.UID,
				v.container,
				terminated.ExitCode,
				terminated.FinishedAt.UTC().Format(time.RFC3339Nano),
			),
			Reason: "app_container_terminated",
		}
	}
	// deletionTimestamp is intentionally ignored: it is intent to terminate,
	// not evidence that the application process has stopped executing.
	return RuntimeTerminationEvidence{Reason: "runtime_still_not_proven_terminated"}
}
