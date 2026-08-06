package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDerivativeTPMIsOwnedByCapacityControl(t *testing.T) {
	_, ok := registry["derivative.tpm"]
	require.False(t, ok)
}
