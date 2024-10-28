package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"time"

	"bsc-node/eth/protocols/bsc"
	"bsc-node/p2p"
	"bsc-node/p2p/enode"
	"bsc-node/p2p/nat"
	"bsc-node/params"

	"github.com/ethereum/go-ethereum/crypto"
)

func main() {
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		log.Fatalf("Failed to generate private key: %v", err)
	}

	bootstrapNodes := []*enode.Node{}
	for _, url := range params.MainnetBootnodes {
		node := enode.MustParse(url)
		bootstrapNodes = append(bootstrapNodes, node)
	}

	protocols := make([]p2p.Protocol, 0, len(bsc.ProtocolVersions))
	var protocolLengths = map[uint]uint64{bsc.Bsc1: 2}
	for _, version := range bsc.ProtocolVersions {
		version := version // Closure

		protocols = append(protocols, p2p.Protocol{
			Name:    bsc.ProtocolName,
			Version: version,
			Length:  protocolLengths[version],
			Run: func(p *p2p.Peer, rw p2p.MsgReadWriter) error {
				fmt.Printf("节点: %s\n", p.Node().String())
				msg, err := rw.ReadMsg()
				if err != nil {
					if err == io.EOF {
						return nil
					}
					return err
				}

				payloadBytes, err := io.ReadAll(msg.Payload)
				if err != nil {
					return err
				}
				// fmt.Printf("从节点 %s 收到消息: \n Code: %d,\n Size: %d\n", p.Node().String(), msg.Code, msg.Size)
				fmt.Println("收到的消息: ", payloadBytes)

				return nil
				// peer := bsc.NewPeer(version, p, rw)
				// defer peer.Close()

				// return backend.RunPeer(peer, func(peer *Peer) error {
				// 	return Handle(backend, peer)
				// })
			},
		})
	}

	config := p2p.Config{
		PrivateKey:      privateKey,
		MaxPeers:        200,
		MaxPendingPeers: 10,
		DialRatio:       3,
		NoDiscovery:     false,
		DiscoveryV4:     true,
		DiscoveryV5:     true,
		Name:            "p2p-monitor",
		ListenAddr:      ":30303",
		NAT:             nat.Any(),
		NoDial:          false,
		EnableMsgEvents: true,
		BootstrapNodes:  bootstrapNodes,
		Protocols:       protocols,
	}
	server := p2p.Server{Config: config}
	if err := server.Start(); err != nil {
		log.Fatalf("Failed to start P2P server: %v", err)
	}
	defer server.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			nodeInfo := server.Peers()
			fmt.Printf("当前节点数: %d\n", len(nodeInfo))
			// for _, peer := range nodeInfo {
			// 	// fmt.Printf("节点: %s\n", peer.Node().String())
			// 	peer.GetMes()
			// }
		case <-ctx.Done():
			return
		}
	}
}
