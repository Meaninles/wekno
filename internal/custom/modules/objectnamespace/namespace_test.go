package objectnamespace

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const testNamespaceID = "74b3d025-5a14-4a6d-b0fc-ff228d0ba98c"

func TestNormalizeAndValidate(t *testing.T) {
	for _, purpose := range []Purpose{
		PurposeKnowledgeObjects,
		PurposeAgentArtifacts,
		PurposeOriginalInputs,
		PurposeProfessionalSkills,
	} {
		prefix, err := NormalizeAndValidate(
			"weknora/__weknora_"+string(purpose)+"_v1__/deployment/dev-local/namespace/"+testNamespaceID+"/",
			purpose,
		)
		require.NoError(t, err)
		require.Equal(
			t,
			"weknora/__weknora_"+string(purpose)+"_v1__/deployment/dev-local/namespace/"+testNamespaceID,
			prefix,
		)
	}
}

func TestNormalizeAndValidateRejectsAmbiguousOrSensitiveRoots(t *testing.T) {
	cases := []string{
		"weknora/",
		"weknora/__weknora_private_knowledge_objects_v1__/deployment/Prod/namespace/" + testNamespaceID,
		"weknora/__weknora_private_knowledge_objects_v1__/deployment/customer_name/namespace/" + testNamespaceID,
		"weknora/__weknora_private_knowledge_objects_v1__/deployment/dev-local/namespace/00000000-0000-0000-0000-000000000000",
		"weknora/__weknora_private_agent_artifacts_v1__/deployment/dev-local/namespace/" + testNamespaceID,
	}
	for _, raw := range cases {
		_, err := NormalizeAndValidate(raw, PurposeKnowledgeObjects)
		require.Error(t, err, raw)
	}
}

func TestKnowledgePrefixFromEnv(t *testing.T) {
	value := "weknora/__weknora_private_knowledge_objects_v1__/deployment/dev-local/namespace/" +
		testNamespaceID + "/"
	t.Setenv("MINIO_PATH_PREFIX", value)
	t.Setenv("OBS_PATH_PREFIX", value)
	for _, provider := range []string{"minio", "obs"} {
		got, err := KnowledgePrefixFromEnv(provider)
		require.NoError(t, err)
		require.Equal(t, strings.TrimSuffix(value, "/"), got)
	}
}
