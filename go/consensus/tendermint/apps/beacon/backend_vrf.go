package beacon

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/tendermint/tendermint/abci/types"

	beacon "github.com/oasisprotocol/oasis-core/go/beacon/api"
	"github.com/oasisprotocol/oasis-core/go/common/cbor"
	"github.com/oasisprotocol/oasis-core/go/common/crypto/signature"
	"github.com/oasisprotocol/oasis-core/go/common/crypto/tuplehash"
	"github.com/oasisprotocol/oasis-core/go/consensus/api/transaction"
	"github.com/oasisprotocol/oasis-core/go/consensus/tendermint/abci"
	"github.com/oasisprotocol/oasis-core/go/consensus/tendermint/api"
	beaconState "github.com/oasisprotocol/oasis-core/go/consensus/tendermint/apps/beacon/state"
	registryState "github.com/oasisprotocol/oasis-core/go/consensus/tendermint/apps/registry/state"
	genesis "github.com/oasisprotocol/oasis-core/go/genesis/api"
)

var vrfAlphaDomainsep = []byte("oasis-core:vrf/alpha")

type backendVRF struct {
	app *beaconApplication
}

func (impl *backendVRF) OnInitChain(
	ctx *api.Context,
	state *beaconState.MutableState,
	params *beacon.ConsensusParameters,
	doc *genesis.Document,
) error {
	// If the backend is configured to use explicitly set epochs, there
	// is nothing further to do.  And yes, this ignores the base epoch,
	// but that's how certain tests are written.
	if params.DebugMockBackend {
		return nil
	}

	// Set the initial epoch.
	baseEpoch := doc.Beacon.Base
	if err := state.SetEpoch(ctx, baseEpoch, ctx.InitialHeight()); err != nil {
		return fmt.Errorf("beacon: failed to set initial epoch: %w", err)
	}

	impl.app.doEmitEpochEvent(ctx, baseEpoch)

	// Arm the initial epoch transition.
	return impl.scheduleEpochTransitionBlock(ctx, state, params.VRFParameters, doc.Beacon.Base+1)
}

func (impl *backendVRF) OnBeginBlock(
	ctx *api.Context,
	state *beaconState.MutableState,
	params *beacon.ConsensusParameters,
	req types.RequestBeginBlock,
) error {
	future, err := state.GetFutureEpoch(ctx)
	if err != nil {
		return fmt.Errorf("beacon: failed to get future epoch: %w", err)
	}

	// Get VRF state.
	vrfState, err := state.VRFState(ctx)
	if err != nil {
		return fmt.Errorf("beacon: failed to get VRF state: %w", err)
	}

	height := ctx.BlockHeight() + 1 // Get the current height.

	// If vrfState == nil, must be the first epoch.  Generate the bootstrap
	// VRF state with a low-quality alpha.
	if vrfState == nil {
		var epoch beacon.EpochTime
		if epoch, _, err = state.GetEpoch(ctx); err != nil {
			return fmt.Errorf("beacon: failed to get current epoch: %w", err)
		}

		vrfState = &beacon.VRFState{
			Epoch:              epoch,
			Alpha:              impl.newLowQualityAlpha(ctx, req, epoch),
			Pi:                 nil,
			AlphaIsHighQuality: false,
			SubmitAfter:        height + params.VRFParameters.ProofSubmissionDelay,
		}
		if err = state.SetVRFState(ctx, vrfState); err != nil {
			return fmt.Errorf("beacon: failed to initialize VRF state: %w", err)
		}
	}

	if future == nil {
		// Must be using the debug mock backend.  Nothing we can do.
		return nil
	}

	switch {
	case future.Height < height:
		// What the fuck, we missed transitioning the epoch?
		ctx.Logger().Error("height mismatch in defered set",
			"height", height,
			"expected_height", future.Height,
		)
		return fmt.Errorf("beacon: height mismatch in defered set")
	case future.Height > height:
		// The epoch transition is scheduled to happen in the grim
		// darkness of the far future.
		return nil
	case future.Height == height: // aka default
		// Time to fire the scheduled epoch transition.
	}

	// Transition the epoch.
	if err = state.SetEpoch(ctx, future.Epoch, height); err != nil {
		return fmt.Errorf("beacon: failed to set epoch: %w", err)
	}
	if err = state.ClearFutureEpoch(ctx); err != nil {
		return fmt.Errorf("beacon: failed to clear future epoch: %w", err)
	}
	if !params.DebugMockBackend {
		if err = impl.scheduleEpochTransitionBlock(ctx, state, params.VRFParameters, future.Epoch+1); err != nil {
			return err
		}
	}
	impl.app.doEmitEpochEvent(ctx, future.Epoch)

	// Generate a new alpha, and update the rest of the state.
	vrfState.Epoch = future.Epoch
	vrfState.AlphaIsHighQuality = uint64(len(vrfState.Pi)) >= params.VRFParameters.AlphaHighQualityThreshold
	vrfState.SubmitAfter = height + params.VRFParameters.ProofSubmissionDelay
	if vrfState.AlphaIsHighQuality {
		// New alpha has enough proofs to allow elections.
		vrfState.Alpha = impl.newHighQualityAlpha(ctx, vrfState)
	} else {
		// New alpha has insufficient proofs to allow elections.
		vrfState.Alpha = impl.newLowQualityAlpha(ctx, req, vrfState.Epoch)
	}
	vrfState.Pi = nil // Clear after new alpha.
	if err = state.SetVRFState(ctx, vrfState); err != nil {
		return fmt.Errorf("beacon: failed to update VRF state: %w", err)
	}

	// Certain things still need entropy:
	//  * All elections with DebugDeterminstic set.
	//  * Tie-breaks for validator elections if insufficient proofs (unlikely).
	var entropy []byte
	if params.DebugDeterministic {
		// UNSAFE/DEBUG - Deterministic beacon.
		//
		// In this mode, while we do the dance to generate alpha/pi,
		// we still use the deterministic entropy source to do elections
		// so that we can rig which nodes go where for tests.  A better
		// approach at some point would be to just allow specifying the
		// winners for each epoch in the genesis state or something,
		// but that's a "someone else, some other time" problem.
		entropy = GetBeacon(future.Epoch, DebugEntropyCtx, DebugEntropy)
	} else {
		// Instead of just using the block hash (which is probably ok),
		// this could consider aggregating all of the beta values from
		// VRF proofs, though that is also merely "probably ok".
		entropy = GetBeacon(future.Epoch, prodEntropyCtx, req.Header.GetLastCommitHash())
	}
	if err = impl.app.onNewBeacon(ctx, entropy); err != nil {
		return fmt.Errorf("beacon: failed to generate debug entropy")
	}

	// Emit the new VRF alpha event.
	ctx.EmitEvent(api.NewEventBuilder(impl.app.Name()).TypedAttribute(&beacon.VRFEvent{
		Epoch:       vrfState.Epoch,
		Alpha:       vrfState.Alpha,
		SubmitAfter: vrfState.SubmitAfter,
	}))

	return nil
}

func (impl *backendVRF) ExecuteTx(
	ctx *api.Context,
	state *beaconState.MutableState,
	params *beacon.ConsensusParameters,
	tx *transaction.Transaction,
) error {
	switch tx.Method {
	case beacon.MethodVRFProve:
		return impl.doProveTx(ctx, state, params, tx)
	case MethodSetEpoch:
		if !params.DebugMockBackend {
			return fmt.Errorf("beacon: method '%s' is disabled via consensus", MethodSetEpoch)
		}
		return impl.doSetEpochTx(ctx, state, tx.Body)
	default:
		return fmt.Errorf("beacon: invalid method: %s", tx.Method)
	}
}

func (impl *backendVRF) doProveTx(
	ctx *api.Context,
	state *beaconState.MutableState,
	params *beacon.ConsensusParameters,
	tx *transaction.Transaction,
) error {
	vrfState, err := state.VRFState(ctx)
	if err != nil {
		return fmt.Errorf("beacon: failed to get VRF state: %w", err)
	}
	if vrfState == nil {
		return fmt.Errorf("beacon: no VRF state")
	}

	// Ensure that the minimum delay has passed since alpha was generated.
	if ctx.BlockHeight()+1 <= vrfState.SubmitAfter {
		return fmt.Errorf("beacon: premature VRF proof")
	}

	// Ensure the tx is from a current valid participant.
	registryState := registryState.NewMutableState(ctx.State())
	node, err := registryState.Node(ctx, ctx.TxSigner())
	if err != nil {
		return fmt.Errorf("beacon: tx not from a node: %v", err)
	}
	if node.VRF == nil {
		return fmt.Errorf("beacon: tx signer missing VRF metadata")
	}

	// Deserialize the tx.
	var proveTx beacon.VRFProve
	if err = cbor.Unmarshal(tx.Body, &proveTx); err != nil {
		return fmt.Errorf("beacon: failed to deserialize prove tx: %w", err)
	}
	if proveTx.Epoch != vrfState.Epoch {
		return fmt.Errorf("beacon: proof for invalid epoch: %d", proveTx.Epoch)
	}

	// Verify the proof.
	proof := signature.Proof{
		PublicKey: node.VRF.ID,
	}
	if err = proof.Proof.UnmarshalBinary(proveTx.Pi); err != nil {
		return fmt.Errorf("beacon: failed to deserialize raw proof: %w", err)
	}
	ok, beta := proof.Verify(vrfState.Alpha)
	if !ok {
		return fmt.Errorf("beacon: failed to verify beta")
	}

	if oldPi := vrfState.Pi[node.ID]; oldPi == nil {
		// Fresh proof, store pi.
		vrfState.Pi[node.ID] = &proof
		if err = state.SetVRFState(ctx, vrfState); err != nil {
			return fmt.Errorf("beacon: failed to update state: %w", err)
		}
	} else {
		// That's odd, the node already submitted a proof, ensure that the
		// betas match, and if not, reject and consider slashing.
		//
		// The beta is checked instead of a byte-for-byte comparison of Pi
		// because proofs are not guaranteed to be deterministic (though
		// the IETF draft happens to produce proofs that are).
		oldBeta := oldPi.UnsafeToHash()
		if !bytes.Equal(oldBeta, beta) {
			return fmt.Errorf("beacon: node attempted to submit a different proof")
		}
	}

	ctx.Logger().Debug("processed VRFProve tx",
		"epoch", proveTx.Epoch,
		"id", node.ID,
	)

	return nil
}

func (impl *backendVRF) doSetEpochTx(
	ctx *api.Context,
	state *beaconState.MutableState,
	txBody []byte,
) error {
	now, _, err := state.GetEpoch(ctx)
	if err != nil {
		return err
	}

	var epoch beacon.EpochTime
	if err := cbor.Unmarshal(txBody, &epoch); err != nil {
		return err
	}

	// XXX: Decide if we want to have this try to wait for enough VRF proofs
	// to be gathered or not.

	if epoch <= now {
		ctx.Logger().Error("explicit epoch transition does not advance time",
			"epoch", now,
			"new_epoch", epoch,
		)
		return fmt.Errorf("beacon: explicit epoch does not advance time")
	}

	height := ctx.BlockHeight() + 1 // Current height is ctx.BlockHeight() + 1

	ctx.Logger().Info("scheduling explicit epoch transition",
		"epoch", epoch,
		"next_height", height+1,
		"is_check_only", ctx.IsCheckOnly(),
	)

	if err := state.SetFutureEpoch(ctx, epoch, height+1); err != nil {
		return err
	}
	return nil
}

func (impl *backendVRF) scheduleEpochTransitionBlock(
	ctx *api.Context,
	state *beaconState.MutableState,
	params *beacon.VRFParameters,
	nextEpoch beacon.EpochTime,
) error {
	// Schedule the epoch transition based on block height.
	nextHeight := int64(nextEpoch) * params.Interval
	return impl.app.scheduleEpochTransitionBlock(ctx, state, nextEpoch, nextHeight)
}

func (impl *backendVRF) initAlphaCommon(
	ctx *api.Context,
	epoch beacon.EpochTime,
) *tuplehash.Hasher {
	h := tuplehash.New256(32, vrfAlphaDomainsep)
	_, _ = h.Write(MustGetChainContext(ctx))
	var epochBytes [8]byte
	binary.BigEndian.PutUint64(epochBytes[:], uint64(epoch))
	_, _ = h.Write(epochBytes[:])
	return h
}

func (impl *backendVRF) newHighQualityAlpha(
	ctx *api.Context,
	vrfState *beacon.VRFState,
) []byte {
	sorted := make([]signature.PublicKey, 0, len(vrfState.Pi))
	for mk := range vrfState.Pi {
		sorted = append(sorted, mk)
	}
	sort.Slice(sorted, func(i, j int) bool {
		return bytes.Compare(sorted[i][:], sorted[j][:]) < 0
	})

	h := impl.initAlphaCommon(ctx, vrfState.Epoch)
	for _, pk := range sorted {
		pi := vrfState.Pi[pk]
		beta := pi.UnsafeToHash() // Ok because invalid proofs don't get stored.
		_, _ = h.Write(beta)
	}
	return h.Sum(nil)
}

func (impl *backendVRF) newLowQualityAlpha(
	ctx *api.Context,
	req types.RequestBeginBlock,
	epoch beacon.EpochTime,
) []byte {
	// This generates a low quality alpha for:
	//  * The bootstrap epoch
	//  * Any subsequent epochs where insufficient nodes submitted VRF proofs
	//
	// This being predictable is ok because the collected proofs from this alpha
	// are only used to generate an actually good alpha, and not for actual
	// elections.
	h := impl.initAlphaCommon(ctx, epoch)
	_, _ = h.Write(req.Header.GetLastCommitHash()) // XXX: Is this really required?
	return h.Sum(nil)
}

// MustGetChainContext returns the global blockchain chain context or panics.
//
// XXX: This lives here because making it a method of `api.Context` import
// loops.
func MustGetChainContext(ctx *api.Context) []byte {
	// Using panic on errors is ok because if this isn't present, something
	// has went horrifically wrong (the key is inserted by the ABCI mux
	// during initialization).
	st := ctx.State()
	b, err := st.Get(ctx, []byte(abci.StateKeyGenesisDigest))
	if err != nil {
		panic("BUG: failed to get chain context: " + err.Error())
	}
	if len(b) == 0 {
		panic("BUG: chain context length is 0")
	}
	return b
}
