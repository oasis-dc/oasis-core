package registration

import (
	"context"
	"sync"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/oasislabs/ekiden/go/common"
	"github.com/oasislabs/ekiden/go/common/crypto/signature"
	fileSigner "github.com/oasislabs/ekiden/go/common/crypto/signature/signers/file"
	"github.com/oasislabs/ekiden/go/common/entity"
	"github.com/oasislabs/ekiden/go/common/identity"
	"github.com/oasislabs/ekiden/go/common/logging"
	"github.com/oasislabs/ekiden/go/common/node"
	"github.com/oasislabs/ekiden/go/ekiden/cmd/common/flags"
	epochtime "github.com/oasislabs/ekiden/go/epochtime/api"
	registry "github.com/oasislabs/ekiden/go/registry/api"
	workerCommon "github.com/oasislabs/ekiden/go/worker/common"
	"github.com/oasislabs/ekiden/go/worker/common/p2p"
)

const (
	cfgRegistrationEntity     = "worker.registration.entity"
	cfgRegistrationPrivateKey = "worker.registration.private_key"
)

// Registration is a service handling worker node registration.
type Registration struct {
	sync.Mutex

	workerCommonCfg *workerCommon.Config

	entityID           signature.PublicKey
	registrationSigner signature.Signer

	epochtime epochtime.Backend
	registry  registry.Backend
	identity  *identity.Identity
	p2p       *p2p.P2P
	ctx       context.Context
	// Bandaid: Idempotent Stop for testing.
	stopped   bool
	quitCh    chan struct{}
	regCh     chan struct{}
	logger    *logging.Logger
	roleHooks []func(*node.Node) error
	consensus common.ConsensusBackend
}

func (r *Registration) doNodeRegistration() {
	// Delay node registration till after the consensus service has
	// finished initial synchronization if applicable.
	if r.consensus != nil {
		select {
		case <-r.quitCh:
			return
		case <-r.consensus.Synced():
		}
	}

	// (re-)register the node on each epoch transition. This doesn't
	// need to be strict block-epoch time, since it just serves to
	// extend the node's expiration.
	ch, sub := r.epochtime.WatchEpochs()
	defer sub.Close()

	regFn := func(epoch epochtime.EpochTime, retry bool) error {
		var off backoff.BackOff

		switch retry {
		case true:
			expBackoff := backoff.NewExponentialBackOff()
			expBackoff.MaxElapsedTime = 0
			off = expBackoff
		case false:
			off = &backoff.StopBackOff{}
		}
		off = backoff.WithContext(off, r.ctx)

		// WARNING: This can potentially infinite loop, on certain
		// "shouldn't be possible" pathological failures.
		//
		// w.ctx being canceled will break out of the loop correctly
		// but it's entirely possible to sit around in an infinite
		// retry loop with no hope of success.
		return backoff.Retry(func() error {
			// Update the epoch if it happens to change while retrying.
			var ok bool
			select {
			case epoch, ok = <-ch:
				if !ok {
					return context.Canceled
				}
			default:
			}

			return r.registerNode(epoch)
		}, off)
	}

	epoch := <-ch
	err := regFn(epoch, true)
	close(r.regCh)
	if err != nil {
		// This by definition is a cancellation.
		return
	}

	for {
		select {
		case <-r.quitCh:
			return
		case epoch = <-ch:
			if err := regFn(epoch, false); err != nil {
				r.logger.Error("failed to re-register node",
					"err", err,
				)
			}
		}
	}
}

// InitialRegistrationCh returns the initial registration channel.
func (r *Registration) InitialRegistrationCh() chan struct{} {
	return r.regCh
}

// RegisterRole enables registering Node roles.
// hook is a callback that does the following:
// - Use AddRole to add a role to the node descriptor
// - Make other changes specific to the role, e.g. setting compute capabilities
func (r *Registration) RegisterRole(hook func(*node.Node) error) {
	r.Lock()
	defer r.Unlock()

	r.roleHooks = append(r.roleHooks, hook)
}

func (r *Registration) registerNode(epoch epochtime.EpochTime) error {
	r.logger.Info("performing node (re-)registration",
		"epoch", epoch,
	)

	addresses, err := r.workerCommonCfg.GetNodeAddresses()
	if err != nil {
		r.logger.Error("failed to register node: unable to get addresses",
			"err", err,
		)
		return err
	}
	identityPublic := r.identity.NodeSigner.Public()
	nodeDesc := node.Node{
		ID:         identityPublic,
		EntityID:   r.entityID,
		Expiration: uint64(epoch) + 2,
		Committee: node.CommitteeInfo{
			Certificate: r.identity.TLSCertificate.Certificate[0],
			Addresses:   addresses,
		},
		P2P:              r.p2p.Info(),
		RegistrationTime: uint64(time.Now().Unix()),
	}
	for _, runtime := range r.workerCommonCfg.Runtimes {
		nodeDesc.Runtimes = append(nodeDesc.Runtimes, &node.Runtime{
			ID: runtime,
		})
	}

	r.Lock()
	defer r.Unlock()

	// Apply worker role hooks:
	for _, h := range r.roleHooks {
		if err := h(&nodeDesc); err != nil {
			r.logger.Error("failed to apply role hook",
				"err", err)
		}
	}

	// Only register node if hooks exist.
	if len(r.roleHooks) > 0 {
		signedNode, err := node.SignNode(r.registrationSigner, registry.RegisterNodeSignatureContext, &nodeDesc)
		if err != nil {
			r.logger.Error("failed to register node: unable to sign node descriptor",
				"err", err,
			)
			return err
		}
		if err := r.registry.RegisterNode(r.ctx, signedNode); err != nil {
			r.logger.Error("failed to register node",
				"err", err,
			)
			return err
		}

		r.logger.Info("node registered with the registry")
	} else {
		r.logger.Info("skipping node registration as no registerted role hooks")
	}

	return nil
}

func getRegistrationSigner(logger *logging.Logger, dataDir string, identity *identity.Identity) (signature.PublicKey, signature.Signer, error) {
	// If the test entity is enabled, use the entity signing key for signing
	// registrations.
	if flags.DebugTestEntity() {
		testEntity, testSigner, _ := entity.TestEntity()
		return testEntity.ID, testSigner, nil
	}

	// Load the registration entity descriptor.
	f := viper.GetString(cfgRegistrationEntity)
	if f == "" {
		// TODO: There are certain configurations (eg: the test client) that
		// spin up workers, which require a registration worker, but don't
		// need it, and do not have an owning entity.  The registration worker
		// should not be initialized in this case.
		return nil, nil, nil
	}

	// Attempt to load the entity descriptor.
	entity, err := entity.LoadDescriptor(f)
	if err != nil {
		return nil, nil, errors.Wrap(err, "worker/registration: failed to load entity descriptor")
	}
	if !entity.AllowEntitySignedNodes {
		return entity.ID, identity.NodeSigner, nil
	}

	logger.Warn("using the entity signing key for node registration")

	// The entity allows self-signed nodes, try to load the entity private key.
	f = viper.GetString(cfgRegistrationPrivateKey)
	if f == "" {
		// No suitable signer with which to sign node registrations.
		return nil, nil, errors.New("worker/registration: no suitable signing key for node registration")
	}

	factory := fileSigner.NewFactory(dataDir, signature.SignerEntity)
	fileFactory := factory.(*fileSigner.Factory)
	entitySigner, err := fileFactory.ForceLoad(f)
	if err != nil {
		return nil, nil, errors.Wrap(err, "worker/registration: failed to load entity signing key")
	}

	return entity.ID, entitySigner, nil
}

// New constructs a new worker node registration service.
func New(
	dataDir string,
	epochtime epochtime.Backend,
	registry registry.Backend,
	identity *identity.Identity,
	consensus common.ConsensusBackend,
	p2p *p2p.P2P,
	workerCommonCfg *workerCommon.Config,
) (*Registration, error) {
	logger := logging.GetLogger("worker/registration")

	entityID, registrationSigner, err := getRegistrationSigner(logger, dataDir, identity)
	if err != nil {
		return nil, err
	}

	r := &Registration{
		workerCommonCfg:    workerCommonCfg,
		entityID:           entityID,
		registrationSigner: registrationSigner,
		epochtime:          epochtime,
		registry:           registry,
		identity:           identity,
		quitCh:             make(chan struct{}),
		regCh:              make(chan struct{}),
		ctx:                context.Background(),
		logger:             logger,
		consensus:          consensus,
		p2p:                p2p,
		roleHooks:          []func(*node.Node) error{},
	}

	return r, nil
}

// Name returns the service name.
func (r *Registration) Name() string {
	return "worker node registration service"
}

// Start starts the registration service.
func (r *Registration) Start() error {
	r.logger.Info("starting node registration service")

	// HACK: This can be ok in certain configurations.
	if r.entityID == nil || r.registrationSigner == nil {
		r.logger.Warn("no entity/signer for this node, registration will NEVER succeed")
		return nil
	}

	go r.doNodeRegistration()

	return nil
}

// Stop halts the service.
func (r *Registration) Stop() {
	if r.stopped {
		return
	}
	r.stopped = true
	close(r.quitCh)
}

// Quit returns a channel that will be closed when the service terminates.
func (r *Registration) Quit() <-chan struct{} {
	return r.quitCh
}

// Cleanup performs the service specific post-termination cleanup.
func (r *Registration) Cleanup() {
}

// RegisterFlags registers the configuration flags with the provided
// command.
func RegisterFlags(cmd *cobra.Command) {
	if !cmd.Flags().Parsed() {
		cmd.Flags().String(cfgRegistrationEntity, "", "Entity to use as the node owner in registrations")
		cmd.Flags().String(cfgRegistrationPrivateKey, "", "Private key to use to sign node registrations")
	}
	for _, v := range []string{
		cfgRegistrationEntity,
		cfgRegistrationPrivateKey,
	} {
		viper.BindPFlag(v, cmd.Flags().Lookup(v)) // nolint: errcheck
	}
}
