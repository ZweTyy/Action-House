package node

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	pb "github.com/ZweTyy/Action-House/src/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type AuctionState struct {
	Start    time.Time
	Duration time.Duration
	Closed   bool

	HighestBid    uint32
	HighestBidder string
	Bidders       map[string]uint32
}

type Node struct {
	pb.UnimplementedAuctionServer

	id    uint
	ports []uint

	mu           sync.Mutex
	state        *AuctionState
	seq          uint32
	isLeader     bool
	leaderID     uint
	leaderConn   *grpc.ClientConn
	leaderClient pb.AuctionClient
	leaderFound  chan struct{}
}

var (
	ctx = context.Background()
	opt = grpc.WithTransportCredentials(insecure.NewCredentials())
)

func Spawn(id uint, ports []uint, durationSec uint) {
	n := &Node{
		id:    id,
		ports: ports,
		state: &AuctionState{
			Start:    time.Now(),
			Duration: time.Duration(durationSec) * time.Second,
			Bidders:  make(map[string]uint32),
		},
		leaderFound: make(chan struct{}, 1),
	}

	go func() {
		server := grpc.NewServer()
		pb.RegisterAuctionServer(server, n)

		addr := fmt.Sprintf("localhost:%d", ports[id])
		lis, err := net.Listen("tcp", addr)
		if err != nil {
			log.Fatalf("Node %d failed to listen on %s: %v", id, addr, err)
		}
		log.Printf("Node %d listening on %s", id, addr)
		if err := server.Serve(lis); err != nil {
			log.Fatalf("Node %d server error: %v", id, err)
		}
	}()
}

func (n *Node) Bid(ctx context.Context, req *pb.BidRequest) (*pb.BidReply, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	for {
		if n.ensureLeaderLocked() != nil {
			return n.handleLeaderBidLocked(req), nil
		}
		if n.leaderClient == nil {
			return &pb.BidReply{Status: "exception", Msg: "no leader"}, nil
		}
		n.mu.Unlock()
		resp, err := n.leaderClient.Bid(ctx, req)
		n.mu.Lock()
		if err != nil {
			log.Printf("Node %d: forward Bid failed, retrying election: %v", n.id, err)
			n.closeLeaderLocked()
			continue
		}
		return resp, nil
	}
}

func (n *Node) Result(ctx context.Context, _ *pb.Void) (*pb.ResultReply, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	for {
		if n.ensureLeaderLocked() != nil {
			return n.handleLeaderResultLocked(), nil
		}
		if n.leaderClient == nil {
			return &pb.ResultReply{Status: "exception"}, nil
		}
		n.mu.Unlock()
		resp, err := n.leaderClient.Result(ctx, &pb.Void{})
		n.mu.Lock()
		if err != nil {
			log.Printf("Node %d: forward Result failed, retrying election: %v", n.id, err)
			n.closeLeaderLocked()
			continue
		}
		return resp, nil
	}
}

func (n *Node) ReplicateUpdate(ctx context.Context, upd *pb.Update) (*pb.Void, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if upd.Seq <= n.seq {
		return &pb.Void{}, nil
	}
	n.seq = upd.Seq
	if upd.Closed {
		n.state.Closed = true
	}
	if upd.HighestBid >= n.state.HighestBid {
		n.state.HighestBid = upd.HighestBid
		n.state.HighestBidder = upd.HighestBidder
		if n.state.Bidders == nil {
			n.state.Bidders = make(map[string]uint32)
		}
		n.state.Bidders[upd.HighestBidder] = upd.HighestBid
	}
	return &pb.Void{}, nil
}

func (n *Node) Election(ctx context.Context, _ *pb.Void) (*pb.Void, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.findLeaderLocked()
	return &pb.Void{}, nil
}

func (n *Node) Leader(ctx context.Context, id *pb.Id) (*pb.Void, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.isLeader = false
	n.closeLeaderLocked()

	targetID := uint(id.Id)
	if targetID >= uint(len(n.ports)) {
		return &pb.Void{}, nil
	}
	addr := fmt.Sprintf("localhost:%d", n.ports[targetID])
	conn, err := grpc.Dial(addr, opt)
	if err != nil {
		log.Printf("Node %d: failed to connect to leader %d: %v", n.id, targetID, err)
		return &pb.Void{}, nil
	}
	n.leaderConn = conn
	n.leaderClient = pb.NewAuctionClient(conn)
	n.leaderID = targetID

	select {
	case n.leaderFound <- struct{}{}:
	default:
	}

	log.Printf("Node %d: learned leader is %d", n.id, n.leaderID)
	return &pb.Void{}, nil
}

func (n *Node) handleLeaderBidLocked(req *pb.BidRequest) *pb.BidReply {
	n.maybeCloseLocked()

	if n.state.Closed {
		return &pb.BidReply{Status: "fail", Msg: "auction closed"}
	}

	if n.state.Bidders == nil {
		n.state.Bidders = make(map[string]uint32)
	}
	last := n.state.Bidders[req.BidderId]
	if req.Amount <= last {
		return &pb.BidReply{Status: "fail", Msg: "must exceed your previous bid"}
	}
	if req.Amount <= n.state.HighestBid {
		return &pb.BidReply{Status: "fail", Msg: "must exceed current highest bid"}
	}

	n.seq++
	upd := &pb.Update{
		Seq:           n.seq,
		HighestBid:    req.Amount,
		HighestBidder: req.BidderId,
		Closed:        false,
	}
	n.broadcastUpdateLocked(upd)

	n.state.Bidders[req.BidderId] = req.Amount
	n.state.HighestBid = req.Amount
	n.state.HighestBidder = req.BidderId

	return &pb.BidReply{Status: "success", Msg: ""}
}

func (n *Node) handleLeaderResultLocked() *pb.ResultReply {
	n.maybeCloseLocked()

	status := "ongoing"
	if n.state.Closed {
		status = "closed"
	}

	return &pb.ResultReply{
		Status:         status,
		HighestBid:     n.state.HighestBid,
		HighestBidder:  n.state.HighestBidder,
	}
}

func (n *Node) maybeCloseLocked() {
	if !n.state.Closed && time.Since(n.state.Start) >= n.state.Duration {
		n.state.Closed = true
		n.seq++
		upd := &pb.Update{
			Seq:           n.seq,
			HighestBid:    n.state.HighestBid,
			HighestBidder: n.state.HighestBidder,
			Closed:        true,
		}
		n.broadcastUpdateLocked(upd)
	}
}

func (n *Node) broadcastUpdateLocked(upd *pb.Update) {
	for i, p := range n.ports {
		if uint(i) == n.id {
			continue
		}
		addr := fmt.Sprintf("localhost:%d", p)
		go func(address string) {
			conn, err := grpc.Dial(address, opt)
			if err != nil {
				return
			}
			defer conn.Close()
			client := pb.NewAuctionClient(conn)
			_, _ = client.ReplicateUpdate(ctx, upd)
		}(addr)
	}
}

func (n *Node) ensureLeaderLocked() *Node {
	if n.isLeader {
		return n
	}
	if n.leaderClient == nil {
		n.findLeaderLocked()
	}
	if n.isLeader {
		return n
	}
	return nil
}

func (n *Node) closeLeaderLocked() {
	if n.leaderConn != nil {
		n.leaderConn.Close()
	}
	n.leaderConn = nil
	n.leaderClient = nil
}

func (n *Node) findLeaderLocked() {
	if n.isLeader || n.leaderClient != nil {
		return
	}

	count := uint(len(n.ports))
	if n.id == count-1 {
		n.becomeLeaderLocked()
		return
	}

	responded := false
	for i := count - 1; i > n.id; i-- {
		addr := fmt.Sprintf("localhost:%d", n.ports[i])
		conn, err := grpc.Dial(addr, opt)
		if err != nil {
			continue
		}
		client := pb.NewAuctionClient(conn)
		_, err = client.Election(ctx, &pb.Void{})
		conn.Close()
		if err != nil {
			continue
		}
		responded = true
	}

	if !responded {
		n.becomeLeaderLocked()
	} else {
		n.mu.Unlock()
		<-n.leaderFound
		n.mu.Lock()
	}
}

func (n *Node) becomeLeaderLocked() {
	n.isLeader = true
	n.leaderID = n.id
	n.closeLeaderLocked()
	log.Printf("Node %d: I am the leader", n.id)

	for i := uint(0); i < n.id; i++ {
		addr := fmt.Sprintf("localhost:%d", n.ports[i])
		go func(a string) {
			conn, err := grpc.Dial(a, opt)
			if err != nil {
				return
			}
			defer conn.Close()
			client := pb.NewAuctionClient(conn)
			_, _ = client.Leader(ctx, &pb.Id{Id: uint32(n.id)})
		}(addr)
	}
}
