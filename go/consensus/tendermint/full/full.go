// Package full implements a full Tendermint consensus node.
package full

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	dbm "github.com/cometbft/cometbft-db"
	"github.com/prometheus/client_golang/prometheus"
	flag "github.com/spf13/pflag"
	"github.com/spf13/viper"
	tmabcitypes "github.com/tendermint/tendermint/abci/types"
	tmconfig "github.com/tendermint/tendermint/config"
	tmmerkle "github.com/tendermint/tendermint/crypto/merkle"
	tmpubsub "github.com/tendermint/tendermint/libs/pubsub"
	tmlight "github.com/tendermint/tendermint/light"
	tmmempool "github.com/tendermint/tendermint/mempool"
	tmnode "github.com/tendermint/tendermint/node"
	tmp2p "github.com/tendermint/tendermint/p2p"
	tmproto "github.com/tendermint/tendermint/proto/tendermint/types"
	tmproxy "github.com/tendermint/tendermint/proxy"
	tmcli "github.com/tendermint/tendermint/rpc/client/local"
	tmstate "github.com/tendermint/tendermint/state"
	tmstatesync "github.com/tendermint/tendermint/statesync"
	tmtypes "github.com/tendermint/tendermint/types"

	beaconAPI "github.com/oasisprotocol/oasis-core/go/beacon/api"
	"github.com/oasisprotocol/oasis-core/go/common/cbor"
	"github.com/oasisprotocol/oasis-core/go/common/crypto/hash"
	"github.com/oasisprotocol/oasis-core/go/common/errors"
	"github.com/oasisprotocol/oasis-core/go/common/identity"
	"github.com/oasisprotocol/oasis-core/go/common/logging"
	"github.com/oasisprotocol/oasis-core/go/common/pubsub"
	"github.com/oasisprotocol/oasis-core/go/common/random"
	"github.com/oasisprotocol/oasis-core/go/config"
	consensusAPI "github.com/oasisprotocol/oasis-core/go/consensus/api"
	"github.com/oasisprotocol/oasis-core/go/consensus/api/transaction"
	"github.com/oasisprotocol/oasis-core/go/consensus/metrics"
	lightP2P "github.com/oasisprotocol/oasis-core/go/consensus/p2p/light"
	"github.com/oasisprotocol/oasis-core/go/consensus/tendermint/abci"
	"github.com/oasisprotocol/oasis-core/go/consensus/tendermint/api"
	tmcommon "github.com/oasisprotocol/oasis-core/go/consensus/tendermint/common"
	"github.com/oasisprotocol/oasis-core/go/consensus/tendermint/crypto"
	"github.com/oasisprotocol/oasis-core/go/consensus/tendermint/db"
	lightAPI "github.com/oasisprotocol/oasis-core/go/consensus/tendermint/light/api"
	genesisAPI "github.com/oasisprotocol/oasis-core/go/genesis/api"
	cmflags "github.com/oasisprotocol/oasis-core/go/oasis-node/cmd/common/flags"
	cmmetrics "github.com/oasisprotocol/oasis-core/go/oasis-node/cmd/common/metrics"
	p2pAPI "github.com/oasisprotocol/oasis-core/go/p2p/api"
	registryAPI "github.com/oasisprotocol/oasis-core/go/registry/api"
	stakingAPI "github.com/oasisprotocol/oasis-core/go/staking/api"
	upgradeAPI "github.com/oasisprotocol/oasis-core/go/upgrade/api"
)

const (
	// Time difference threshold used when considering if node is done with
	// initial syncing. If difference is greater than the specified threshold
	// the node is considered not yet synced.
	// NOTE: this is only used during the initial sync.
	syncWorkerLastBlockTimeDiffThreshold = 1 * time.Minute

	minUpgradeStopWaitPeriod = 5 * time.Second

	// tmSubscriberID is the subscriber identifier used for all internal Tendermint pubsub
	// subscriptions. If any other subscriber IDs need to be derived they will be under this prefix.
	tmSubscriberID = "oasis-core"
)

var (
	_ api.Backend = (*fullService)(nil)

	labelTendermint = prometheus.Labels{"backend": "tendermint"}

	// Flags has the configuration flags.
	Flags = flag.NewFlagSet("", flag.ContinueOnError)
)

// fullService implements a full Tendermint node.
type fullService struct { // nolint: maligned
	sync.Mutex
	*commonNode

	p2p p2pAPI.Service

	upgrader      upgradeAPI.Backend
	node          *tmnode.Node
	client        *tmcli.Local
	blockNotifier *pubsub.Broker
	failMonitor   *failMonitor

	submissionMgr consensusAPI.SubmissionManager

	genesisProvider genesisAPI.Provider
	syncedCh        chan struct{}
	quitCh          chan struct{}

	startFn  func() error
	stopOnce sync.Once

	nextSubscriberID uint64
}

// Implements consensusAPI.Backend.
func (t *fullService) Start() error {
	if t.started() {
		return fmt.Errorf("tendermint: service already started")
	}

	switch t.initialized() {
	case true:
		if err := t.commonNode.start(); err != nil {
			return err
		}
		if err := t.startFn(); err != nil {
			return err
		}
		if err := t.node.Start(); err != nil {
			return fmt.Errorf("tendermint: failed to start service: %w", err)
		}

		// Make sure the quit channel is closed when the node shuts down.
		go func() {
			select {
			case <-t.quitCh:
			case <-t.node.Quit():
				select {
				case <-t.quitCh:
				default:
					close(t.quitCh)
				}
			}
		}()

		// Start event dispatchers for all the service clients.
		t.serviceClientsWg.Add(len(t.serviceClients))
		for _, svc := range t.serviceClients {
			go t.serviceClientWorker(t.ctx, svc)
		}
		// Start sync checker.
		go t.syncWorker()
		// Start block notifier.
		go t.blockNotifierWorker()
		// Optionally start metrics updater.
		if cmmetrics.Enabled() {
			go t.metrics()
		}
	case false:
		close(t.syncedCh)
	}

	t.commonNode.finishStart()

	return nil
}

// Implements consensusAPI.Backend.
func (t *fullService) Quit() <-chan struct{} {
	return t.quitCh
}

// Implements consensusAPI.Backend.
func (t *fullService) Stop() {
	if !t.initialized() || !t.started() {
		return
	}

	t.stopOnce.Do(func() {
		t.failMonitor.markCleanShutdown()
		if err := t.node.Stop(); err != nil {
			t.Logger.Error("Error on stopping node", err)
		}

		t.commonNode.stop()
	})
}

// Implements consensusAPI.Backend.
func (t *fullService) Synced() <-chan struct{} {
	return t.syncedCh
}

// Implements consensusAPI.Backend.
func (t *fullService) SupportedFeatures() consensusAPI.FeatureMask {
	return consensusAPI.FeatureServices | consensusAPI.FeatureFullNode
}

// Implements consensusAPI.Backend.
func (t *fullService) SubmitTx(ctx context.Context, tx *transaction.SignedTransaction) error {
	if _, err := t.submitTx(ctx, tx); err != nil {
		return err
	}
	return nil
}

// Implements consensusAPI.Backend.
func (t *fullService) SubmitTxNoWait(ctx context.Context, tx *transaction.SignedTransaction) error {
	return t.broadcastTxRaw(cbor.Marshal(tx))
}

// Implements consensusAPI.Backend.
func (t *fullService) SubmitTxWithProof(ctx context.Context, tx *transaction.SignedTransaction) (*transaction.Proof, error) {
	data, err := t.submitTx(ctx, tx)
	if err != nil {
		return nil, err
	}

	txs, err := t.GetTransactions(ctx, data.Height)
	if err != nil {
		return nil, err
	}

	if data.Index >= uint32(len(txs)) {
		return nil, fmt.Errorf("tendermint: invalid transaction index")
	}

	// Tendermint Merkle tree is computed over hashes and not over transactions.
	hashes := make([][]byte, 0, len(txs))
	for _, tx := range txs {
		hash := sha256.Sum256(tx)
		hashes = append(hashes, hash[:])
	}

	_, proofs := tmmerkle.ProofsFromByteSlices(hashes)

	return &transaction.Proof{
		Height:   data.Height,
		RawProof: cbor.Marshal(proofs[data.Index]),
	}, nil
}

func (t *fullService) submitTx(ctx context.Context, tx *transaction.SignedTransaction) (*tmtypes.EventDataTx, error) {
	// Subscribe to the transaction being included in a block.
	data := cbor.Marshal(tx)
	query := tmtypes.EventQueryTxFor(data)
	subID := t.newSubscriberID()
	txSub, err := t.subscribe(subID, query)
	if err != nil {
		return nil, err
	}
	if ptrSub, ok := txSub.(*tendermintPubsubBuffer).tmSubscription.(*tmpubsub.Subscription); ok && ptrSub == nil {
		t.Logger.Debug("broadcastTx: service has shut down. Cancel our context to recover")
		<-ctx.Done()
		return nil, ctx.Err()
	}

	defer t.unsubscribe(subID, query) // nolint: errcheck

	// Subscribe to the transaction becoming invalid.
	txHash := hash.NewFromBytes(data)

	recheckCh, recheckSub, err := t.mux.WatchInvalidatedTx(txHash)
	if err != nil {
		return nil, err
	}
	defer recheckSub.Close()

	// First try to broadcast.
	if err := t.broadcastTxRaw(data); err != nil {
		return nil, err
	}

	// Wait for the transaction to be included in a block.
	select {
	case v := <-recheckCh:
		return nil, v
	case v := <-txSub.Out():
		data := v.Data().(tmtypes.EventDataTx)
		if result := data.Result; !result.IsOK() {
			return nil, errors.FromCode(result.GetCodespace(), result.GetCode(), result.GetLog())
		}
		return &data, nil
	case <-txSub.Cancelled():
		return nil, context.Canceled
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *fullService) broadcastTxRaw(data []byte) error {
	// We could use t.client.BroadcastTxSync but that is annoying as it
	// doesn't give you the right fields when CheckTx fails.
	mp := t.node.Mempool()

	// Submit the transaction to mempool and wait for response.
	ch := make(chan *tmabcitypes.Response, 1)
	err := mp.CheckTx(tmtypes.Tx(data), func(rsp *tmabcitypes.Response) {
		ch <- rsp
		close(ch)
	}, tmmempool.TxInfo{})
	switch err {
	case nil:
	case tmmempool.ErrTxInCache:
		// Transaction already in the mempool or was recently there.
		return consensusAPI.ErrDuplicateTx
	default:
		return fmt.Errorf("tendermint: failed to submit to local mempool: %w", err)
	}

	rsp := <-ch
	if result := rsp.GetCheckTx(); !result.IsOK() {
		return errors.FromCode(result.GetCodespace(), result.GetCode(), result.GetLog())
	}

	return nil
}

func (t *fullService) newSubscriberID() string {
	return fmt.Sprintf("%s/subscriber-%d", tmSubscriberID, atomic.AddUint64(&t.nextSubscriberID, 1))
}

// Implements consensusAPI.Backend.
func (t *fullService) SubmitEvidence(ctx context.Context, evidence *consensusAPI.Evidence) error {
	var protoEv tmproto.Evidence
	if err := protoEv.Unmarshal(evidence.Meta); err != nil {
		return fmt.Errorf("tendermint: malformed evidence while unmarshalling: %w", err)
	}

	ev, err := tmtypes.EvidenceFromProto(&protoEv)
	if err != nil {
		return fmt.Errorf("tendermint: malformed evidence while converting: %w", err)
	}

	if _, err := t.client.BroadcastEvidence(ctx, ev); err != nil {
		return fmt.Errorf("tendermint: broadcast evidence failed: %w", err)
	}

	return nil
}

func (t *fullService) subscribe(subscriber string, query tmpubsub.Query) (tmtypes.Subscription, error) {
	// Note: The tendermint documentation claims using SubscribeUnbuffered can
	// freeze the server, however, the buffered Subscribe can drop events, and
	// force-unsubscribe the channel if processing takes too long.

	subFn := func() (tmtypes.Subscription, error) {
		sub, err := t.node.EventBus().SubscribeUnbuffered(t.ctx, subscriber, query)
		if err != nil {
			return nil, err
		}
		// Oh yes, this can actually return a nil subscription even though the
		// error was also nil if the node is just shutting down.
		if sub == (*tmpubsub.Subscription)(nil) {
			return nil, context.Canceled
		}
		return newTendermintPubsubBuffer(sub), nil
	}

	if t.started() {
		return subFn()
	}

	// The node doesn't exist until it's started since, creating the node
	// triggers replay, InitChain, and etc.
	t.Logger.Debug("Subscribe: node not available yet, blocking",
		"subscriber", subscriber,
		"query", query,
	)

	// XXX/yawning: As far as I can tell just blocking here is safe as
	// ever single consumer of the API subscribes from a go routine.
	select {
	case <-t.startedCh:
	case <-t.ctx.Done():
		return nil, t.ctx.Err()
	}

	return subFn()
}

func (t *fullService) unsubscribe(subscriber string, query tmpubsub.Query) error {
	if t.started() {
		return t.node.EventBus().Unsubscribe(t.ctx, subscriber, query)
	}

	return fmt.Errorf("tendermint: unsubscribe called with no backing service")
}

// Implements consensusAPI.Backend.
func (t *fullService) SubmissionManager() consensusAPI.SubmissionManager {
	return t.submissionMgr
}

// Implements consensusAPI.Backend.
func (t *fullService) GetUnconfirmedTransactions(ctx context.Context) ([][]byte, error) {
	mempoolTxs := t.node.Mempool().ReapMaxTxs(-1)
	txs := make([][]byte, 0, len(mempoolTxs))
	for _, v := range mempoolTxs {
		txs = append(txs, v[:])
	}
	return txs, nil
}

// Implements consensusAPI.Backend.
func (t *fullService) GetStatus(ctx context.Context) (*consensusAPI.Status, error) {
	status, err := t.commonNode.GetStatus(ctx)
	if err != nil {
		return nil, err
	}
	status.Status = consensusAPI.StatusStateSyncing

	status.P2P = &consensusAPI.P2PStatus{}
	status.P2P.PubKey = t.identity.P2PSigner.Public()
	if status.P2P.Addresses, err = t.GetAddresses(); err != nil {
		return nil, err
	}

	if t.started() {
		// Check if node is synced.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.Synced():
			status.Status = consensusAPI.StatusStateReady
		default:
		}

		// List of consensus peers.
		tmpeers := t.node.Switch().Peers().List()
		peers := make([]string, 0, len(tmpeers))
		for _, tmpeer := range tmpeers {
			p := string(tmpeer.ID()) + "@" + tmpeer.RemoteAddr().String()
			peers = append(peers, p)
		}

		status.P2P.Peers = peers
		status.P2P.PeerID = string(t.node.NodeInfo().ID())
	}

	return status, nil
}

// Implements consensusAPI.Backend.
func (t *fullService) GetNextBlockState(ctx context.Context) (*consensusAPI.NextBlockState, error) {
	if !t.started() {
		return nil, fmt.Errorf("tendermint: not yet started")
	}

	rs := t.node.ConsensusState().GetRoundState()
	nbs := &consensusAPI.NextBlockState{
		Height: rs.Height,

		NumValidators: uint64(rs.Validators.Size()),
		VotingPower:   uint64(rs.Validators.TotalVotingPower()),
	}

	for i, val := range rs.Validators.Validators {
		var vote consensusAPI.Vote
		valNode, err := t.Registry().GetNodeByConsensusAddress(ctx, &registryAPI.ConsensusAddressQuery{
			Height:  consensusAPI.HeightLatest,
			Address: val.Address,
		})
		if err == nil {
			vote.NodeID = valNode.ID
			vote.EntityID = valNode.EntityID
			vote.EntityAddress = stakingAPI.NewAddress(valNode.EntityID)
		}
		vote.VotingPower = uint64(val.VotingPower)

		if prevote := rs.Votes.Prevotes(rs.Round).GetByIndex(int32(i)); prevote != nil {
			nbs.Prevotes.Votes = append(nbs.Prevotes.Votes, vote)
			nbs.Prevotes.VotingPower = nbs.Prevotes.VotingPower + vote.VotingPower
		}
		if precommit := rs.Votes.Precommits(rs.Round).GetByIndex(int32(i)); precommit != nil {
			nbs.Precommits.Votes = append(nbs.Precommits.Votes, vote)
			nbs.Precommits.VotingPower = nbs.Precommits.VotingPower + vote.VotingPower
		}
	}
	nbs.Prevotes.Ratio = float64(nbs.Prevotes.VotingPower) / float64(nbs.VotingPower)
	nbs.Precommits.Ratio = float64(nbs.Precommits.VotingPower) / float64(nbs.VotingPower)

	return nbs, nil
}

// Implements consensusAPI.Backend.
func (t *fullService) RegisterP2PService(p2p p2pAPI.Service) error {
	t.Lock()
	defer t.Unlock()
	if t.p2p != nil {
		return fmt.Errorf("p2p service already registered")
	}
	t.p2p = p2p

	// Register consensus protocol server.
	t.p2p.RegisterProtocolServer(lightP2P.NewServer(t.p2p, t.genesis.ChainContext(), t))

	return nil
}

// Implements consensusAPI.Backend.
func (t *fullService) WatchBlocks(ctx context.Context) (<-chan *consensusAPI.Block, pubsub.ClosableSubscription, error) {
	ch, sub, err := t.WatchTendermintBlocks()
	if err != nil {
		return nil, nil, err
	}
	mapCh := make(chan *consensusAPI.Block)
	go func() {
		defer close(mapCh)

		for {
			select {
			case tmBlk, ok := <-ch:
				if !ok {
					return
				}

				mapCh <- api.NewBlock(tmBlk)
			case <-ctx.Done():
				return
			}
		}
	}()

	return mapCh, sub, nil
}

// Implements consensusAPI.Backend.
func (t *fullService) WatchTendermintBlocks() (<-chan *tmtypes.Block, *pubsub.Subscription, error) {
	typedCh := make(chan *tmtypes.Block)
	sub := t.blockNotifier.Subscribe()
	sub.Unwrap(typedCh)

	return typedCh, sub, nil
}

func (t *fullService) lazyInit() error { // nolint: gocyclo
	if t.initialized() {
		return nil
	}

	var err error

	// Create Tendermint application mux.
	var pruneCfg abci.PruneConfig
	pruneStrat := config.GlobalConfig.Consensus.Prune.Strategy
	if err = pruneCfg.Strategy.FromString(pruneStrat); err != nil {
		return err
	}
	pruneCfg.NumKept = config.GlobalConfig.Consensus.Prune.NumKept
	pruneCfg.PruneInterval = config.GlobalConfig.Consensus.Prune.Interval
	const minPruneInterval = 1 * time.Second
	if pruneCfg.PruneInterval < minPruneInterval {
		pruneCfg.PruneInterval = minPruneInterval
	}

	appConfig := &abci.ApplicationConfig{
		DataDir:                   filepath.Join(t.dataDir, tmcommon.StateDir),
		StorageBackend:            db.GetBackendName(),
		Pruning:                   pruneCfg,
		HaltEpoch:                 beaconAPI.EpochTime(config.GlobalConfig.Consensus.HaltEpoch),
		HaltHeight:                config.GlobalConfig.Consensus.HaltHeight,
		MinGasPrice:               config.GlobalConfig.Consensus.MinGasPrice,
		OwnTxSigner:               t.identity.NodeSigner.Public(),
		DisableCheckpointer:       config.GlobalConfig.Consensus.Checkpointer.Disabled,
		CheckpointerCheckInterval: config.GlobalConfig.Consensus.Checkpointer.CheckInterval,
		InitialHeight:             uint64(t.genesis.Height),
		ChainContext:              t.genesis.ChainContext(),
	}
	t.mux, err = abci.NewApplicationServer(t.ctx, t.upgrader, appConfig)
	if err != nil {
		return err
	}

	// Tendermint needs the on-disk directories to be present when
	// launched like this, so create the relevant sub-directories
	// under the node DataDir.
	tendermintDataDir := filepath.Join(t.dataDir, tmcommon.StateDir)
	if err = tmcommon.InitDataDir(tendermintDataDir); err != nil {
		return err
	}

	// Convert addresses and public keys to Tendermint form.
	persistentPeers, err := tmcommon.ConsensusAddressesToTendermint(config.GlobalConfig.Consensus.P2P.PersistentPeer)
	if err != nil {
		return fmt.Errorf("tendermint: failed to convert persistent peer addresses: %w", err)
	}
	seeds, err := tmcommon.ConsensusAddressesToTendermint(config.GlobalConfig.P2P.Seeds)
	if err != nil {
		return fmt.Errorf("tendermint: failed to convert seed addresses: %w", err)
	}
	sentryUpstreamAddrs, err := tmcommon.ConsensusAddressesToTendermint(config.GlobalConfig.Consensus.SentryUpstreamAddresses)
	if err != nil {
		return fmt.Errorf("tendermint: failed to convert sentry upstream addresses: %w", err)
	}
	unconditionalPeers, err := tmcommon.PublicKeysToTendermint(config.GlobalConfig.Consensus.P2P.UnconditionalPeer)
	if err != nil {
		return fmt.Errorf("tendermint: failed to convert unconditional peer public keys: %w", err)
	}

	// Create Tendermint node.
	tenderConfig := tmconfig.DefaultConfig()
	_ = viper.Unmarshal(&tenderConfig)
	tenderConfig.SetRoot(tendermintDataDir)
	timeoutCommit := t.genesis.Consensus.Parameters.TimeoutCommit
	emptyBlockInterval := t.genesis.Consensus.Parameters.EmptyBlockInterval
	tenderConfig.Consensus.TimeoutCommit = timeoutCommit
	tenderConfig.Consensus.SkipTimeoutCommit = t.genesis.Consensus.Parameters.SkipTimeoutCommit
	tenderConfig.Consensus.CreateEmptyBlocks = true
	tenderConfig.Consensus.CreateEmptyBlocksInterval = emptyBlockInterval
	tenderConfig.Consensus.DebugUnsafeReplayRecoverCorruptedWAL = config.GlobalConfig.Consensus.Debug.UnsafeReplayRecoverCorruptedWAL && cmflags.DebugDontBlameOasis()
	tenderConfig.Mempool.Version = tmconfig.MempoolV1
	tenderConfig.Instrumentation.Prometheus = true
	tenderConfig.Instrumentation.PrometheusListenAddr = ""
	tenderConfig.TxIndex.Indexer = "null"
	tenderConfig.P2P.ListenAddress = config.GlobalConfig.Consensus.ListenAddress
	tenderConfig.P2P.ExternalAddress = config.GlobalConfig.Consensus.ExternalAddress
	tenderConfig.P2P.PexReactor = !config.GlobalConfig.Consensus.P2P.DisablePeerExchange
	tenderConfig.P2P.MaxNumInboundPeers = config.GlobalConfig.Consensus.P2P.MaxNumInboundPeers
	tenderConfig.P2P.MaxNumOutboundPeers = config.GlobalConfig.Consensus.P2P.MaxNumOutboundPeers
	tenderConfig.P2P.SendRate = config.GlobalConfig.Consensus.P2P.SendRate
	tenderConfig.P2P.RecvRate = config.GlobalConfig.Consensus.P2P.RecvRate
	tenderConfig.P2P.PersistentPeers = strings.Join(persistentPeers, ",")
	tenderConfig.P2P.PersistentPeersMaxDialPeriod = config.GlobalConfig.Consensus.P2P.PersistenPeersMaxDialPeriod
	tenderConfig.P2P.UnconditionalPeerIDs = strings.Join(unconditionalPeers, ",")
	tenderConfig.P2P.Seeds = strings.Join(seeds, ",")
	tenderConfig.P2P.AddrBookStrict = !(config.GlobalConfig.Consensus.Debug.P2PAddrBookLenient && cmflags.DebugDontBlameOasis())
	tenderConfig.P2P.AllowDuplicateIP = config.GlobalConfig.Consensus.Debug.P2PAllowDuplicateIP && cmflags.DebugDontBlameOasis()
	tenderConfig.RPC.ListenAddress = ""

	if len(sentryUpstreamAddrs) > 0 {
		t.Logger.Info("Acting as a tendermint sentry", "addrs", sentryUpstreamAddrs)

		// Append upstream addresses to persistent, private and unconditional peers.
		tenderConfig.P2P.PersistentPeers += "," + strings.Join(sentryUpstreamAddrs, ",")

		var sentryUpstreamIDs []string
		for _, addr := range sentryUpstreamAddrs {
			parts := strings.Split(addr, "@")
			if len(parts) != 2 {
				return fmt.Errorf("malformed sentry upstream address: %s", addr)
			}
			sentryUpstreamIDs = append(sentryUpstreamIDs, parts[0])
		}

		sentryUpstreamIDsStr := strings.Join(sentryUpstreamIDs, ",")
		tenderConfig.P2P.PrivatePeerIDs += "," + sentryUpstreamIDsStr
		tenderConfig.P2P.UnconditionalPeerIDs += "," + sentryUpstreamIDsStr
	}

	if !tenderConfig.P2P.PexReactor {
		t.Logger.Info("pex reactor disabled",
			logging.LogEvent, api.LogEventPeerExchangeDisabled,
		)
	}

	tendermintPV, err := crypto.LoadOrGeneratePrivVal(tendermintDataDir, t.identity.ConsensusSigner)
	if err != nil {
		return err
	}

	tmGenDoc, err := api.GetTendermintGenesisDocument(t.genesisProvider)
	if err != nil {
		t.Logger.Error("failed to obtain genesis document",
			"err", err,
		)
		return err
	}
	tendermintGenesisProvider := func() (*tmtypes.GenesisDoc, error) {
		return tmGenDoc, nil
	}

	dbProvider, err := db.GetProvider()
	if err != nil {
		t.Logger.Error("failed to obtain database provider",
			"err", err,
		)
		return err
	}

	// HACK: Wrap the provider so we can extract the state database handle. This is required because
	// Tendermint does not expose a way to access the state database and we need it to bypass some
	// stupid things like pagination on the in-process "client".
	wrapDbProvider := func(dbCtx *tmnode.DBContext) (dbm.DB, error) {
		rawDB, derr := dbProvider(dbCtx)
		if derr != nil {
			return nil, derr
		}
		db := db.WithCloser(rawDB, t.dbCloser)

		switch dbCtx.ID {
		case "state":
			// Tendermint state database.
			t.stateStore = tmstate.NewStore(db, tmstate.StoreOptions{})
		case "blockstore":
			// Tendermint blockstore database.
			t.blockStoreDB = db
		default:
		}

		return db, nil
	}

	// HACK: tmnode.NewNode() triggers block replay and or ABCI chain
	// initialization, instead of t.node.Start().  This is a problem
	// because at the time that lazyInit() is called, none of the ABCI
	// applications are registered.
	//
	// Defer actually initializing the node till after everything
	// else is setup.
	t.startFn = func() (err error) {
		defer func() {
			// The node constructor can panic early in case an error occurrs during block replay as
			// the fail monitor is not yet initialized in that case. Propagate the error.
			if p := recover(); p != nil {
				switch pt := p.(type) {
				case error:
					err = pt
				default:
					err = fmt.Errorf("%v", pt)
				}
			}
		}()

		// Configure state sync if enabled.
		var stateProvider tmstatesync.StateProvider
		if config.GlobalConfig.Consensus.StateSync.Enabled {
			t.Logger.Info("state sync enabled")

			t.Lock()
			if t.p2p == nil {
				t.Unlock()
				t.Logger.Info("failed to create state sync client", "err", "p2p disabled")
				return fmt.Errorf("failed to create state sync client: p2p disabled")
			}
			t.Unlock()

			// Enable state sync in the configuration.
			tenderConfig.StateSync.Enable = true
			tenderConfig.StateSync.TrustHash = config.GlobalConfig.Consensus.StateSync.TrustHash

			// Create new state sync state provider.
			cfg := lightAPI.ClientConfig{
				GenesisDocument: tmGenDoc,
				TrustOptions: tmlight.TrustOptions{
					Period: config.GlobalConfig.Consensus.StateSync.TrustPeriod,
					Height: int64(config.GlobalConfig.Consensus.StateSync.TrustHeight),
					Hash:   tenderConfig.StateSync.TrustHashBytes(),
				},
			}
			if stateProvider, err = newStateProvider(t.ctx, t.genesis.ChainContext(), cfg, t.p2p); err != nil {
				t.Logger.Error("failed to create state sync state provider",
					"err", err,
				)
				return fmt.Errorf("failed to create state sync state provider: %w", err)
			}
		}

		t.node, err = tmnode.NewNode(tenderConfig,
			tendermintPV,
			&tmp2p.NodeKey{PrivKey: crypto.SignerToTendermint(t.identity.P2PSigner)},
			tmproxy.NewLocalClientCreator(t.mux.Mux()),
			tendermintGenesisProvider,
			wrapDbProvider,
			tmnode.DefaultMetricsProvider(tenderConfig.Instrumentation),
			tmcommon.NewLogAdapter(!config.GlobalConfig.Consensus.LogDebug),
			tmnode.StateProvider(stateProvider),
		)
		if err != nil {
			return fmt.Errorf("tendermint: failed to create node: %w", err)
		}
		if t.stateStore == nil {
			// Sanity check for the above wrapDbProvider hack in case the DB provider changes.
			return fmt.Errorf("tendermint: internal error: state database not set")
		}
		t.client = tmcli.New(t.node)
		t.failMonitor = newFailMonitor(t.ctx, t.Logger, t.node.ConsensusState().Wait)

		// Register a halt hook that handles upgrades gracefully.
		t.RegisterHaltHook(func(ctx context.Context, blockHeight int64, epoch beaconAPI.EpochTime, err error) {
			if !errors.Is(err, upgradeAPI.ErrStopForUpgrade) {
				return
			}

			// Mark this as a clean shutdown and request the node to stop gracefully.
			t.failMonitor.markCleanShutdown()

			// Wait before stopping to give time for P2P messages to propagate. Sleep for at least
			// minUpgradeStopWaitPeriod or the configured commit timeout.
			t.Logger.Info("waiting a bit before stopping the node for upgrade")
			waitPeriod := minUpgradeStopWaitPeriod
			if tc := t.genesis.Consensus.Parameters.TimeoutCommit; tc > waitPeriod {
				waitPeriod = tc
			}
			time.Sleep(waitPeriod)

			go func() {
				// Sleep another period so there is some time between when consensus shuts down and
				// when all the other services start shutting down.
				//
				// Randomize the period so that not all nodes shut down at the same time.
				delay := random.GetRandomValueFromInterval(0.5, rand.Float64(), config.GlobalConfig.Consensus.UpgradeStopDelay)
				time.Sleep(delay)

				t.Logger.Info("stopping the node for upgrade")
				t.Stop()

				// Close the quit channel early to force the node to stop. This is needed because
				// the Tendermint node will otherwise never quit.
				close(t.quitCh)
			}()
		})

		return nil
	}

	return nil
}

func (t *fullService) syncWorker() {
	checkSyncFn := func() (isSyncing bool, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("tendermint: node disappeared, terminated?")
			}
		}()

		return t.node.ConsensusReactor().WaitSync(), nil
	}

	for {
		select {
		case <-t.node.Quit():
			return
		case <-time.After(1 * time.Second):
			isFastSyncing, err := checkSyncFn()
			if err != nil {
				t.Logger.Error("Failed to poll FastSync",
					"err", err,
				)
				return
			}
			if !isFastSyncing {
				// Check latest block time.
				tmBlock, err := t.GetTendermintBlock(t.ctx, consensusAPI.HeightLatest)
				if err != nil {
					t.Logger.Error("Failed to get tendermint block",
						"err", err,
					)
					return
				}

				if tmBlock == nil {
					continue
				}

				now := time.Now()
				// Latest block within threshold.
				if now.Sub(tmBlock.Header.Time) < syncWorkerLastBlockTimeDiffThreshold {
					t.Logger.Info("Tendermint Node finished initial sync")
					close(t.syncedCh)
					return
				}

				t.Logger.Debug("Node still syncing",
					"currentTime", now,
					"latestBlockTime", tmBlock.Time,
					"diff", now.Sub(tmBlock.Time),
				)
			}
		}
	}
}

func (t *fullService) blockNotifierWorker() {
	sub, err := t.node.EventBus().SubscribeUnbuffered(t.ctx, tmSubscriberID, tmtypes.EventQueryNewBlock)
	if err != nil {
		t.Logger.Error("failed to subscribe to new block events",
			"err", err,
		)
		return
	}
	// Oh yes, this can actually return a nil subscription even though the error was also
	// nil if the node is just shutting down.
	if sub == (*tmpubsub.Subscription)(nil) {
		return
	}
	defer t.node.EventBus().Unsubscribe(t.ctx, tmSubscriberID, tmtypes.EventQueryNewBlock) // nolint: errcheck

	for {
		select {
		// Should not return on t.ctx.Done()/t.node.Quit() as that could lead to a deadlock.
		case <-sub.Cancelled():
			return
		case v := <-sub.Out():
			ev := v.Data().(tmtypes.EventDataNewBlock)
			t.blockNotifier.Broadcast(ev.Block)
		}
	}
}

// metrics updates oasis_consensus metrics by checking last accepted block info.
func (t *fullService) metrics() {
	ch, sub, err := t.WatchTendermintBlocks()
	if err != nil {
		return
	}
	defer sub.Close()

	// Tendermint uses specific public key encoding.
	pubKey := t.identity.ConsensusSigner.Public()
	myAddr := []byte(crypto.PublicKeyToTendermint(&pubKey).Address())
	for {
		var blk *tmtypes.Block
		select {
		case <-t.node.Quit():
			return
		case blk = <-ch:
		}

		// Was block proposed by our node.
		if bytes.Equal(myAddr, blk.ProposerAddress) {
			metrics.ProposedBlocks.With(labelTendermint).Inc()
		}

		// Was block voted for by our node. Ignore if there was no previous block.
		if blk.LastCommit != nil {
			for _, sig := range blk.LastCommit.Signatures {
				if sig.Absent() || sig.BlockIDFlag == tmtypes.BlockIDFlagNil {
					// Vote is missing, ignore.
					continue
				}

				if bytes.Equal(myAddr, sig.ValidatorAddress) {
					metrics.SignedBlocks.With(labelTendermint).Inc()
					break
				}
			}
		}
	}
}

// New creates a new Tendermint consensus backend.
func New(
	ctx context.Context,
	dataDir string,
	identity *identity.Identity,
	upgrader upgradeAPI.Backend,
	genesisProvider genesisAPI.Provider,
) (consensusAPI.Backend, error) {
	commonNode, err := newCommonNode(ctx, dataDir, identity, genesisProvider)
	if err != nil {
		return nil, err
	}

	t := &fullService{
		commonNode:      commonNode,
		upgrader:        upgrader,
		blockNotifier:   pubsub.NewBroker(false),
		genesisProvider: genesisProvider,
		syncedCh:        make(chan struct{}),
		quitCh:          make(chan struct{}),
	}
	// Common node needs access to parent struct for initializing consensus services.
	t.commonNode.parentNode = t

	t.Logger.Info("starting a full consensus node")

	// Create the submission manager.
	pd, err := consensusAPI.NewStaticPriceDiscovery(config.GlobalConfig.Consensus.Submission.GasPrice)
	if err != nil {
		return nil, fmt.Errorf("tendermint: failed to create submission manager: %w", err)
	}
	t.submissionMgr = consensusAPI.NewSubmissionManager(t, pd, config.GlobalConfig.Consensus.Submission.MaxFee)

	if err := t.lazyInit(); err != nil {
		return nil, fmt.Errorf("lazy init: %w", err)
	}

	return t, t.initialize()
}
