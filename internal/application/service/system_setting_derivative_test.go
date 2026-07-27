package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDerivativeTPMSettingHasSafeGlobalDefaultsAndBounds(t *testing.T) {
	spec, ok := registry["derivative.tpm"]
	require.True(t, ok)
	require.Equal(t, "int", spec.Type)
	require.Equal(t, "WEKNORA_DERIVATIVE_TPM", spec.EnvName)
	require.EqualValues(t, 20_000, spec.Default)
	require.False(t, spec.RequiresRestart)

	for _, valid := range []any{int64(100), int64(20_000), int64(2_000_000)} {
		require.NoError(t, validateRegistryEntry("derivative.tpm", valid))
	}
	for _, invalid := range []any{int64(99), int64(2_000_001), "20000", nil} {
		require.Error(t, validateRegistryEntry("derivative.tpm", invalid))
	}
}
