// Package objectnamespace validates the durable object-key roots used by this
// deployment.  The deployment UUID is deliberately stable and non-secret: it
// separates independently managed clusters that happen to share a bucket,
// while the purpose marker separates data with different retention and access
// rules inside one cluster.
package objectnamespace

import (
	"fmt"
	"os"
	"strings"

	"github.com/Tencent/WeKnora/internal/custom/modules/plannedfile"
	"github.com/google/uuid"
)

type Purpose string

const (
	PurposeKnowledgeObjects Purpose = "private_knowledge_objects"
	PurposeAgentArtifacts   Purpose = "private_agent_artifacts"
	PurposeOriginalInputs   Purpose = "claude_sdk_original_inputs"
)

func root(purpose Purpose) (string, error) {
	switch purpose {
	case PurposeKnowledgeObjects, PurposeAgentArtifacts, PurposeOriginalInputs:
		return "weknora/__weknora_" + string(purpose) + "_v1__/deployment", nil
	default:
		return "", fmt.Errorf("unsupported object namespace purpose %q", purpose)
	}
}

// NormalizeAndValidate accepts one exact deployment-scoped namespace:
//
//	weknora/__weknora_<purpose>_v1__/deployment/<dns-label>/namespace/<uuid>/
//
// Deployment labels and UUIDs are infrastructure identities. Customer names,
// usernames, source filenames, credentials, or other business data must never
// be used in either segment.
func NormalizeAndValidate(raw string, purpose Purpose) (string, error) {
	prefix, err := plannedfile.NormalizePrefix(raw)
	if err != nil {
		return "", fmt.Errorf("invalid %s object path prefix: %w", purpose, err)
	}
	expectedRoot, err := root(purpose)
	if err != nil {
		return "", err
	}
	parts := strings.Split(strings.Trim(prefix, "/"), "/")
	if len(parts) != 6 ||
		strings.Join(parts[:3], "/") != expectedRoot ||
		parts[4] != "namespace" {
		return "", fmt.Errorf(
			"%s object path prefix must be %s/<deployment>/namespace/<uuid>/",
			purpose,
			expectedRoot,
		)
	}
	deployment := parts[3]
	if deployment == "" ||
		len(deployment) > 63 ||
		deployment != strings.ToLower(deployment) ||
		deployment[0] < 'a' || deployment[0] > 'z' ||
		!isDNSLabel(deployment) {
		return "", fmt.Errorf(
			"%s deployment name must be a lowercase DNS label starting with a letter",
			purpose,
		)
	}
	namespaceID, err := uuid.Parse(parts[5])
	if err != nil ||
		namespaceID.String() != parts[5] ||
		namespaceID == uuid.Nil ||
		namespaceID.Variant() != uuid.RFC4122 ||
		namespaceID.Version() < 1 ||
		namespaceID.Version() > 5 {
		return "", fmt.Errorf("%s namespace must be a canonical non-zero UUID", purpose)
	}
	return prefix, nil
}

// KnowledgePrefixFromEnv returns the required durable namespace for the two
// object providers used by this deployment. A generic bucket root is rejected:
// it cannot distinguish production, test, restore drills, or a second cluster.
func KnowledgePrefixFromEnv(provider string) (string, error) {
	var name string
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "minio":
		name = "MINIO_PATH_PREFIX"
	case "obs":
		name = "OBS_PATH_PREFIX"
	default:
		return "", fmt.Errorf("knowledge object namespace is unsupported for provider %q", provider)
	}
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return "", fmt.Errorf("%s is required for durable knowledge objects", name)
	}
	return NormalizeAndValidate(raw, PurposeKnowledgeObjects)
}

func isDNSLabel(value string) bool {
	if value == "" || value[len(value)-1] == '-' {
		return false
	}
	for _, ch := range value {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
			continue
		}
		return false
	}
	return true
}
