package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChatQueueSettingsHaveHotSafeDefaultsAndBounds(t *testing.T) {
	cases := []struct {
		key        string
		defaultVal any
		valid      []any
		invalid    []any
	}{
		{
			key: "chat.queue.default_max_waiting", defaultVal: int64(500),
			valid:   []any{int64(0), int64(500), int64(100000)},
			invalid: []any{int64(-1), int64(100001), "500"},
		},
		{
			key: "chat.queue.max_waiting_per_user", defaultVal: int64(3),
			valid:   []any{int64(1), int64(3), int64(1000)},
			invalid: []any{int64(0), int64(1001), "3"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.key, func(t *testing.T) {
			spec, ok := registry[testCase.key]
			require.True(t, ok)
			require.Equal(t, "int", spec.Type)
			require.Equal(t, testCase.defaultVal, spec.Default)
			require.False(t, spec.RequiresRestart)
			for _, value := range testCase.valid {
				require.NoError(t, validateRegistryEntry(testCase.key, value))
			}
			for _, value := range testCase.invalid {
				require.Error(t, validateRegistryEntry(testCase.key, value))
			}
		})
	}

	enabled, ok := registry["chat.queue.enabled"]
	require.True(t, ok)
	require.Equal(t, "bool", enabled.Type)
	require.Equal(t, true, enabled.Default)
	require.False(t, enabled.RequiresRestart)
}
