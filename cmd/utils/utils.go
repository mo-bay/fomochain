package utils

import (
	"github.com/69th-byte/sdexchain/eth"
	"github.com/69th-byte/sdexchain/eth/downloader"
	"github.com/69th-byte/sdexchain/ethstats"
	"github.com/69th-byte/sdexchain/les"
	"github.com/69th-byte/sdexchain/node"
	"github.com/69th-byte/sdexchain/tomox"
	"github.com/69th-byte/sdexchain/tomoxlending"
	whisper "github.com/69th-byte/sdexchain/whisper/whisperv6"
)

// RegisterEthService adds an Ethereum client to the stack.
func RegisterEthService(stack *node.Node, cfg *eth.Config) {
	var err error
	if cfg.SyncMode == downloader.LightSync {
		err = stack.Register(func(ctx *node.ServiceContext) (node.Service, error) {
			return les.New(ctx, cfg)
		})
	} else {
		err = stack.Register(func(ctx *node.ServiceContext) (node.Service, error) {
			var tomoXServ *tomox.TomoX
			ctx.Service(&tomoXServ)
			var lendingServ *tomoxlending.Lending
			ctx.Service(&lendingServ)
			fullNode, err := eth.New(ctx, cfg, tomoXServ, lendingServ)
			if fullNode != nil && cfg.LightServ > 0 {
				ls, _ := les.NewLesServer(fullNode, cfg)
				fullNode.AddLesServer(ls)
			}
			return fullNode, err
		})
	}
	if err != nil {
		Fatalf("Failed to register the Ethereum service: %v", err)
	}
}

// RegisterShhService configures Whisper and adds it to the given node.
func RegisterShhService(stack *node.Node, cfg *whisper.Config) {
	if err := stack.Register(func(n *node.ServiceContext) (node.Service, error) {
		return whisper.New(cfg), nil
	}); err != nil {
		Fatalf("Failed to register the Whisper service: %v", err)
	}
}

// RegisterEthStatsService configures the Ethereum Stats daemon and adds it to
// th egiven node.
func RegisterEthStatsService(stack *node.Node, url string) {
	if err := stack.Register(func(ctx *node.ServiceContext) (node.Service, error) {
		// Retrieve both eth and les services
		var ethServ *eth.Ethereum
		ctx.Service(&ethServ)

		var lesServ *les.LightEthereum
		ctx.Service(&lesServ)

		return ethstats.New(url, ethServ, lesServ)
	}); err != nil {
		Fatalf("Failed to register the Ethereum Stats service: %v", err)
	}
}

func RegisterTomoXService(stack *node.Node, cfg *tomox.Config) {
	tomoX := tomox.New(cfg)
	if err := stack.Register(func(n *node.ServiceContext) (node.Service, error) {
		return tomoX, nil
	}); err != nil {
		Fatalf("Failed to register the TomoX service: %v", err)
	}

	// register tomoxlending service
	if err := stack.Register(func(n *node.ServiceContext) (node.Service, error) {
		return tomoxlending.New(tomoX), nil
	}); err != nil {
		Fatalf("Failed to register the TomoXLending service: %v", err)
	}
}
