package storagemigration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHashAndRewind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.bin")
	require.NoError(t, os.WriteFile(path, []byte("bounded-stream"), 0o600))
	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()

	digest, size, err := hashAndRewind(file)
	require.NoError(t, err)
	require.EqualValues(t, 14, size)
	require.Equal(t, "58022ba5d774629729de3364d46ccb004ef309252be47f57def4b7e042b3e252", digest)

	buffer := make([]byte, 14)
	_, err = file.Read(buffer)
	require.NoError(t, err)
	require.Equal(t, "bounded-stream", string(buffer))
}

func TestQuoteIdentifier(t *testing.T) {
	require.Equal(t, `"messages"`, quoteIdentifier("messages"))
	require.Equal(t, `"a""b"`, quoteIdentifier(`a"b`))
}
