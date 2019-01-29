package roothash

import (
	"context"

	"google.golang.org/grpc"

	"github.com/oasislabs/ekiden/go/common/crypto/signature"
	pb "github.com/oasislabs/ekiden/go/grpc/roothash"
	"github.com/oasislabs/ekiden/go/roothash/api"
	"github.com/oasislabs/ekiden/go/roothash/api/block"
)

var _ pb.RootHashServer = (*grpcServer)(nil)

type grpcServer struct {
	backend api.Backend
}

func (s *grpcServer) GetLatestBlock(ctx context.Context, req *pb.LatestBlockRequest) (*pb.LatestBlockResponse, error) {
	var id signature.PublicKey
	if err := id.UnmarshalBinary(req.GetRuntimeId()); err != nil {
		return nil, err
	}

	blk, err := s.backend.GetLatestBlock(ctx, id)
	if err != nil {
		return nil, err
	}

	return &pb.LatestBlockResponse{Block: blk.ToProto()}, nil
}

func (s *grpcServer) GetBlocks(req *pb.BlockRequest, stream pb.RootHash_GetBlocksServer) error {
	var id signature.PublicKey
	if err := id.UnmarshalBinary(req.GetRuntimeId()); err != nil {
		return err
	}

	ch, sub, err := s.backend.WatchBlocks(id)
	if err != nil {
		return err
	}
	defer sub.Close()

	return grpcSendBlocks(ch, stream)
}

func (s *grpcServer) GetBlocksSince(req *pb.BlockSinceRequest, stream pb.RootHash_GetBlocksSinceServer) error {
	var id signature.PublicKey
	if err := id.UnmarshalBinary(req.GetRuntimeId()); err != nil {
		return err
	}

	var round block.Round
	if err := round.UnmarshalBinary(req.GetRound()); err != nil {
		return err
	}

	ch, sub, err := s.backend.WatchBlocksSince(id, round)
	if err != nil {
		return err
	}
	defer sub.Close()

	return grpcSendBlocks(ch, stream)
}

// NewGRPCServer initializes and registers a gRPC root hash server
// backed by the provided backend.
func NewGRPCServer(srv *grpc.Server, backend api.Backend) {
	s := &grpcServer{
		backend: backend,
	}
	pb.RegisterRootHashServer(srv, s)
}

type blockSender interface {
	Context() context.Context
	Send(*pb.BlockResponse) error
}

func grpcSendBlocks(ch <-chan *block.Block, stream blockSender) error {
	for {
		var blk *block.Block
		var ok bool

		select {
		case blk, ok = <-ch:
		case <-stream.Context().Done():
		}
		if !ok {
			break
		}

		resp := &pb.BlockResponse{
			Block: blk.ToProto(),
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}

	return nil
}
