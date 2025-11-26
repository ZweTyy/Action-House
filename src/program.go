package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"sort"

	nodepkg "github.com/ZweTyy/Action-House/src/node"
	pb "github.com/ZweTyy/Action-House/src/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	ports []uint
	opt   = grpc.WithTransportCredentials(insecure.NewCredentials())
	ctx   = context.Background()
	conn  *grpc.ClientConn
	cli   pb.AuctionClient
)

func main() {
	thisPort := flag.Uint("port", 0, "The port of this process.")
	asNode := flag.Bool("node", false, "Run as node (server) instead of client.")
	logFile := flag.String("log", "", "Log file (default: stdout).")
	duration := flag.Uint("duration", 100, "Auction duration in seconds (nodes only).")
	clientID := flag.String("id", "client", "Client id (for bidding).")

	flag.Parse()

	if *asNode && *thisPort == 0 {
		fmt.Println("When running as node you must specify -port")
		os.Exit(1)
	}

	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err != nil {
			log.Fatalf("Failed to open log file: %v", err)
		}
		log.SetOutput(f)
	}

	for _, p := range flag.Args() {
		var port uint
		if _, err := fmt.Sscanf(p, "%d", &port); err != nil {
			log.Fatalf("Failed to parse port %q: %v", p, err)
		}
		if *asNode && port == *thisPort {
			continue
		}
		ports = append(ports, port)
	}
	if len(ports) == 0 {
		log.Fatalf("No node ports specified")
	}

	if *asNode {
		runNode(*thisPort, *duration)
	} else {
		runClient(*clientID)
	}
}

func runNode(thisPort uint, duration uint) {
	ports = append(ports, thisPort)
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })

	var id uint
	for i, p := range ports {
		if p == thisPort {
			id = uint(i)
			break
		}
	}
	log.Printf("Starting node id=%d on port=%d, peers=%v, duration=%ds", id, thisPort, ports, duration)
	nodepkg.Spawn(id, ports, duration)

	select {}
}

func runClient(clientID string) {
	log.Printf("Client started with id=%s, nodes=%v", clientID, ports)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Split(bufio.ScanWords)

	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				log.Fatalf("Input error: %v", err)
			}
			return
		}
		cmd := scanner.Text()

		switch cmd {
		case "bid":
			if !scanner.Scan() {
				log.Println("Missing amount")
				continue
			}
			var amount uint
			if _, err := fmt.Sscanf(scanner.Text(), "%d", &amount); err != nil {
				log.Printf("Bad amount: %v", err)
				continue
			}
			ok, msg := sendBid(clientID, uint32(amount))
			log.Printf("Bid %d -> %s (%s)", amount, ok, msg)

		case "result":
			status, amt, bidder := sendResult()
			log.Printf("Result: status=%s, highest_bid=%d, highest_bidder=%s",
				status, amt, bidder)

		case "quit", "exit":
			return

		default:
			log.Printf("Unknown command: %s (use: bid <amt>, result, quit)", cmd)
		}
	}
}

func sendBid(bidderID string, amount uint32) (string, string) {
	for {
		findConnection()
		reply, err := cli.Bid(ctx, &pb.BidRequest{
			BidderId: bidderID,
			Amount:   amount,
		})
		if err != nil {
			closeConn()
			continue
		}
		return reply.Status, reply.Msg
	}
}

func sendResult() (string, uint32, string) {
	for {
		findConnection()
		res, err := cli.Result(ctx, &pb.Void{})
		if err != nil {
			closeConn()
			continue
		}
		return res.Status, res.HighestBid, res.HighestBidder
	}
}

func closeConn() {
	if conn != nil {
		conn.Close()
	}
	conn, cli = nil, nil
}

func findConnection() {
	if cli != nil {
		return
	}

	for {
		port := ports[rand.Intn(len(ports))]
		addr := fmt.Sprintf("localhost:%d", port)
		log.Printf("Trying node %s", addr)

		c, err := grpc.Dial(addr, opt)
		if err != nil {
			log.Printf("Dial failed: %v", err)
			continue
		}
		client := pb.NewAuctionClient(c)

		if _, err := client.Result(ctx, &pb.Void{}); err != nil {
			log.Printf("Health check to %s failed: %v", addr, err)
			c.Close()
			continue
		}

		conn = c
		cli = client
		log.Printf("Connected to %s", addr)
		return
	}
}
