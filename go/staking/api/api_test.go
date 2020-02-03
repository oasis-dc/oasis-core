package api

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/oasislabs/oasis-core/go/common/quantity"
)

func TestConsensusParameters(t *testing.T) {
	require := require.New(t)

	// Default consensus parameters.
	var emptyParams ConsensusParameters
	require.Error(emptyParams.SanityCheck(), "default consensus parameters should be invalid")

	// Valid thresholds.
	validThresholds := map[ThresholdKind]quantity.Quantity{
		KindEntity:    *quantity.NewQuantity(),
		KindValidator: *quantity.NewQuantity(),
		KindCompute:   *quantity.NewQuantity(),
		KindStorage:   *quantity.NewQuantity(),
	}
	validThresholdsParams := ConsensusParameters{
		Thresholds:    validThresholds,
		FeeWeightVote: 1,
	}
	require.NoError(validThresholdsParams.SanityCheck(), "consensus parameters with valid thresholds should be valid")

	// NOTE: There is currently no way to construct invalid thresholds.

	// Degenerate fee weights.
	degenerateFeeWeights := ConsensusParameters{
		FeeWeightVote:    25,
		FeeWeightPropose: -25,
	}
	require.Error(degenerateFeeWeights.SanityCheck(), "consensus parameters with degenerate fee weights should be invalid")
}
