package peermgmt

import (
	"context"
	"fmt"
	"sync"

	"github.com/libp2p/go-libp2p/core"
	"github.com/libp2p/go-libp2p/core/peer"
	manet "github.com/multiformats/go-multiaddr/net"

	"github.com/oasisprotocol/oasis-core/go/common/logging"
	"github.com/oasisprotocol/oasis-core/go/common/node"
	cmSync "github.com/oasisprotocol/oasis-core/go/common/sync"
	consensus "github.com/oasisprotocol/oasis-core/go/consensus/api"
	"github.com/oasisprotocol/oasis-core/go/p2p/api"
)

type peerRegistry struct {
	logger *logging.Logger

	consensus    consensus.Backend
	chainContext string

	mu            sync.Mutex
	peers         map[core.PeerID]*peer.AddrInfo
	protocolPeers map[core.ProtocolID]map[core.PeerID]*peer.AddrInfo
	topicPeers    map[string]map[core.PeerID]*peer.AddrInfo

	initCh   chan struct{}
	initOnce sync.Once

	startOne cmSync.One
}

func newPeerRegistry(c consensus.Backend, chainContext string) *peerRegistry {
	l := logging.GetLogger("p2p/peer-manager/registry")

	return &peerRegistry{
		logger:        l,
		consensus:     c,
		chainContext:  chainContext,
		peers:         make(map[core.PeerID]*peer.AddrInfo),
		protocolPeers: make(map[core.ProtocolID]map[core.PeerID]*peer.AddrInfo),
		topicPeers:    make(map[string]map[core.PeerID]*peer.AddrInfo),
		initCh:        make(chan struct{}),
		startOne:      cmSync.NewOne(),
	}
}

// Implements api.PeerRegistry.
func (r *peerRegistry) Initialized() <-chan struct{} {
	return r.initCh
}

// Implements api.PeerRegistry.
func (r *peerRegistry) NumPeers() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.peers)
}

func (r *peerRegistry) protocolPeersInfo(p core.ProtocolID) []*peer.AddrInfo {
	r.mu.Lock()
	defer r.mu.Unlock()

	pp := r.protocolPeers[p]
	peers := make([]*peer.AddrInfo, 0, len(pp))
	for _, peer := range pp {
		peers = append(peers, peer)
	}

	return peers
}

func (r *peerRegistry) topicPeersInfo(topic string) []*peer.AddrInfo {
	r.mu.Lock()
	defer r.mu.Unlock()

	tp := r.topicPeers[topic]
	peers := make([]*peer.AddrInfo, 0, len(tp))
	for _, peer := range tp {
		peers = append(peers, peer)
	}

	return peers
}

// start starts watching the registry for node changes and assigns nodes to protocols and topics
// according to their roles.
func (r *peerRegistry) start() {
	r.startOne.TryStart(r.watch)
}

// stop stops watching the registry.
func (r *peerRegistry) stop() {
	r.startOne.TryStop()
}

func (r *peerRegistry) watch(ctx context.Context) {
	if r.consensus == nil {
		return
	}

	// Wait for consensus sync before proceeding.
	select {
	case <-r.consensus.Synced():
	case <-ctx.Done():
		return
	}

	// Listen to nodes on epoch transitions.
	nodeListCh, nlSub, err := r.consensus.Registry().WatchNodeList(ctx)
	if err != nil {
		r.logger.Error("failed to watch registry for node list changes",
			"err", err,
		)
		return
	}
	defer nlSub.Close()

	// Listen to nodes on node events.
	nodeCh, nSub, err := r.consensus.Registry().WatchNodes(ctx)
	if err != nil {
		r.logger.Error("failed to watch registry for node changes",
			"err", err,
		)
		return
	}
	defer nSub.Close()

	for {
		select {
		case nodes := <-nodeListCh:
			r.handleNodes(nodes.Nodes, true)

		case nodeEv := <-nodeCh:
			if nodeEv.IsRegistration {
				r.handleNodes([]*node.Node{nodeEv.Node}, false)
			}

		case <-ctx.Done():
			return
		}
	}
}

// handleNodes updates protocols and topics supported by the given nodes and resets them if needed.
func (r *peerRegistry) handleNodes(nodes []*node.Node, reset bool) {
	defer r.initOnce.Do(func() {
		close(r.initCh)
	})

	type peerData struct {
		info      *peer.AddrInfo
		protocols map[core.ProtocolID]struct{}
		topics    map[string]struct{}
	}

	peers := make(map[core.PeerID]*peerData)
	for _, n := range nodes {
		info, err := p2pInfoToAddrInfo(&n.P2P)
		if err != nil {
			r.logger.Error("failed to convert node to node info",
				"err", err,
				"node_id", n.ID,
			)
			continue
		}

		protocols, topics := r.inspectNode(n)

		peers[info.ID] = &peerData{info, protocols, topics}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Remove previous state.
	if reset {
		r.peers = make(map[core.PeerID]*peer.AddrInfo)
		r.protocolPeers = make(map[core.ProtocolID]map[core.PeerID]*peer.AddrInfo)
		r.topicPeers = make(map[string]map[core.PeerID]*peer.AddrInfo)
	}

	// Add/update new peers.
	for p, data := range peers {
		// Remove old protocols/topics.
		for _, peers := range r.protocolPeers {
			delete(peers, p)
		}
		for _, peers := range r.topicPeers {
			delete(peers, p)
		}

		// Add new ones.
		for protocol := range data.protocols {
			peers, ok := r.protocolPeers[protocol]
			if !ok {
				peers = make(map[core.PeerID]*peer.AddrInfo)
				r.protocolPeers[protocol] = peers
			}
			peers[p] = data.info
		}
		for topic := range data.topics {
			peers, ok := r.topicPeers[topic]
			if !ok {
				peers = make(map[core.PeerID]*peer.AddrInfo)
				r.topicPeers[topic] = peers
			}
			peers[p] = data.info
		}

		// Update the address, as it might have changed.
		r.peers[p] = data.info
	}
}

func (r *peerRegistry) inspectNode(n *node.Node) (map[core.ProtocolID]struct{}, map[string]struct{}) {
	pMap := make(map[core.ProtocolID]struct{})
	tMap := make(map[string]struct{})

	nodeHandlers.RLock()
	defer nodeHandlers.RUnlock()

	for _, h := range nodeHandlers.l {
		for _, p := range h.Protocols(n, r.chainContext) {
			pMap[p] = struct{}{}
		}
		for _, t := range h.Topics(n, r.chainContext) {
			tMap[t] = struct{}{}
		}
	}

	return pMap, tMap
}

func p2pInfoToAddrInfo(pi *node.P2PInfo) (*peer.AddrInfo, error) {
	var (
		ai  peer.AddrInfo
		err error
	)
	if ai.ID, err = api.PublicKeyToPeerID(pi.ID); err != nil {
		return nil, fmt.Errorf("failed to extract public key from node P2P ID: %w", err)
	}
	for _, nodeAddr := range pi.Addresses {
		addr, err := manet.FromNetAddr(nodeAddr.ToTCPAddr())
		if err != nil {
			return nil, fmt.Errorf("failed to convert address to libp2p format: %w", err)
		}
		ai.Addrs = append(ai.Addrs, addr)
	}

	return &ai, nil
}
