// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package eth implements the Ethereum protocol.
package eth

import (
	"errors"
	"fmt"
	"math/big"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/69th-byte/sdexchain/tomoxlending"

	"github.com/69th-byte/sdexchain/accounts/abi/bind"
	"github.com/69th-byte/sdexchain/common/hexutil"
	"github.com/69th-byte/sdexchain/core/state"
	"github.com/69th-byte/sdexchain/eth/filters"
	"github.com/69th-byte/sdexchain/rlp"

	"bytes"

	"github.com/69th-byte/sdexchain/accounts"
	"github.com/69th-byte/sdexchain/common"
	"github.com/69th-byte/sdexchain/consensus"
	"github.com/69th-byte/sdexchain/consensus/ethash"
	"github.com/69th-byte/sdexchain/consensus/posv"
	"github.com/69th-byte/sdexchain/contracts"
	contractValidator "github.com/69th-byte/sdexchain/contracts/validator/contract"
	"github.com/69th-byte/sdexchain/core"
	"github.com/69th-byte/sdexchain/core/bloombits"

	//"github.com/69th-byte/sdexchain/core/state"
	"github.com/69th-byte/sdexchain/core/types"
	"github.com/69th-byte/sdexchain/core/vm"
	"github.com/69th-byte/sdexchain/eth/downloader"
	"github.com/69th-byte/sdexchain/eth/gasprice"
	"github.com/69th-byte/sdexchain/ethdb"
	"github.com/69th-byte/sdexchain/event"
	"github.com/69th-byte/sdexchain/internal/ethapi"
	"github.com/69th-byte/sdexchain/log"
	"github.com/69th-byte/sdexchain/miner"
	"github.com/69th-byte/sdexchain/node"
	"github.com/69th-byte/sdexchain/p2p"
	"github.com/69th-byte/sdexchain/params"
	"github.com/69th-byte/sdexchain/rpc"
	"github.com/69th-byte/sdexchain/tomox"
)

type LesServer interface {
	Start(srvr *p2p.Server)
	Stop()
	Protocols() []p2p.Protocol
	SetBloomBitsIndexer(bbIndexer *core.ChainIndexer)
}

// Ethereum implements the Ethereum full node service.
type Ethereum struct {
	config      *Config
	chainConfig *params.ChainConfig

	// Channel for shutting down the service
	shutdownChan chan bool // Channel for shutting down the ethereum

	// Handlers
	txPool          *core.TxPool
	orderPool       *core.OrderPool
	lendingPool     *core.LendingPool
	blockchain      *core.BlockChain
	protocolManager *ProtocolManager
	lesServer       LesServer

	// DB interfaces
	chainDb ethdb.Database // Block chain database

	eventMux       *event.TypeMux
	engine         consensus.Engine
	accountManager *accounts.Manager

	bloomRequests chan chan *bloombits.Retrieval // Channel receiving bloom data retrieval requests
	bloomIndexer  *core.ChainIndexer             // Bloom indexer operating during block imports

	ApiBackend *EthApiBackend

	miner     *miner.Miner
	gasPrice  *big.Int
	etherbase common.Address

	networkId     uint64
	netRPCService *ethapi.PublicNetAPI

	lock    sync.RWMutex // Protects the variadic fields (e.g. gas price and etherbase)
	TomoX   *tomox.TomoX
	Lending *tomoxlending.Lending
}

func (s *Ethereum) AddLesServer(ls LesServer) {
	s.lesServer = ls
	ls.SetBloomBitsIndexer(s.bloomIndexer)
}

// New creates a new Ethereum object (including the
// initialisation of the common Ethereum object)
func New(ctx *node.ServiceContext, config *Config, tomoXServ *tomox.TomoX, lendingServ *tomoxlending.Lending) (*Ethereum, error) {
	if config.SyncMode == downloader.LightSync {
		return nil, errors.New("can't run eth.Ethereum in light sync mode, use les.LightEthereum")
	}
	if !config.SyncMode.IsValid() {
		return nil, fmt.Errorf("invalid sync mode %d", config.SyncMode)
	}
	chainDb, err := CreateDB(ctx, config, "chaindata")
	if err != nil {
		return nil, err
	}
	chainConfig, genesisHash, genesisErr := core.SetupGenesisBlock(chainDb, config.Genesis)
	if _, ok := genesisErr.(*params.ConfigCompatError); genesisErr != nil && !ok {
		return nil, genesisErr
	}

	log.Info("Initialised chain configuration", "config", chainConfig)

	eth := &Ethereum{
		config:         config,
		chainDb:        chainDb,
		chainConfig:    chainConfig,
		eventMux:       ctx.EventMux,
		accountManager: ctx.AccountManager,
		engine:         CreateConsensusEngine(ctx, &config.Ethash, chainConfig, chainDb),
		shutdownChan:   make(chan bool),
		networkId:      config.NetworkId,
		gasPrice:       config.GasPrice,
		etherbase:      config.Etherbase,
		bloomRequests:  make(chan chan *bloombits.Retrieval),
		bloomIndexer:   NewBloomIndexer(chainDb, params.BloomBitsBlocks),
	}
	// Inject TomoX Service into main Eth Service.
	if tomoXServ != nil {
		eth.TomoX = tomoXServ
	}
	if lendingServ != nil {
		eth.Lending = lendingServ
	}
	log.Info("Initialising Ethereum protocol", "versions", ProtocolVersions, "network", config.NetworkId)

	if !config.SkipBcVersionCheck {
		bcVersion := core.GetBlockChainVersion(chainDb)
		if bcVersion != core.BlockChainVersion && bcVersion != 0 {
			return nil, fmt.Errorf("Blockchain DB version mismatch (%d / %d). Run geth upgradedb.\n", bcVersion, core.BlockChainVersion)
		}
		core.WriteBlockChainVersion(chainDb, core.BlockChainVersion)
	}
	var (
		vmConfig    = vm.Config{EnablePreimageRecording: config.EnablePreimageRecording}
		cacheConfig = &core.CacheConfig{Disabled: config.NoPruning, TrieNodeLimit: config.TrieCache, TrieTimeLimit: config.TrieTimeout}
	)
	if eth.chainConfig.Posv != nil {
		c := eth.engine.(*posv.Posv)
		c.GetTomoXService = func() posv.TradingService {
			return eth.TomoX
		}
		c.GetLendingService = func() posv.LendingService {
			return eth.Lending
		}
	}
	eth.blockchain, err = core.NewBlockChainEx(chainDb, tomoXServ.GetLevelDB(), cacheConfig, eth.chainConfig, eth.engine, vmConfig)
	if err != nil {
		return nil, err
	}
	// Rewind the chain in case of an incompatible config upgrade.
	if compat, ok := genesisErr.(*params.ConfigCompatError); ok {
		log.Warn("Rewinding chain to upgrade configuration", "err", compat)
		eth.blockchain.SetHead(compat.RewindTo)
		core.WriteChainConfig(chainDb, genesisHash, chainConfig)
	}
	eth.bloomIndexer.Start(eth.blockchain)

	if config.TxPool.Journal != "" {
		config.TxPool.Journal = ctx.ResolvePath(config.TxPool.Journal)
	}
	eth.txPool = core.NewTxPool(config.TxPool, eth.chainConfig, eth.blockchain)
	eth.orderPool = core.NewOrderPool(eth.chainConfig, eth.blockchain)
	eth.lendingPool = core.NewLendingPool(eth.chainConfig, eth.blockchain)
	if common.RollbackHash != common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000") {
		curBlock := eth.blockchain.CurrentBlock()
		prevBlock := eth.blockchain.GetBlockByHash(common.RollbackHash)

		if curBlock.NumberU64() > prevBlock.NumberU64() {
			for ; curBlock != nil && curBlock.NumberU64() != prevBlock.NumberU64(); curBlock = eth.blockchain.GetBlock(curBlock.ParentHash(), curBlock.NumberU64()-1) {
				eth.blockchain.Rollback([]common.Hash{curBlock.Hash()})
			}
		}

		if prevBlock != nil {
			err := eth.blockchain.SetHead(prevBlock.NumberU64())
			if err != nil {
				log.Crit("Err Rollback", "err", err)
				return nil, err
			}
		}
	}

	if eth.protocolManager, err = NewProtocolManagerEx(eth.chainConfig, config.SyncMode, config.NetworkId, eth.eventMux, eth.txPool, eth.orderPool, eth.lendingPool, eth.engine, eth.blockchain, chainDb); err != nil {
		return nil, err
	}
	eth.miner = miner.New(eth, eth.chainConfig, eth.EventMux(), eth.engine, ctx.GetConfig().AnnounceTxs)
	eth.miner.SetExtra(makeExtraData(config.ExtraData))

	eth.ApiBackend = &EthApiBackend{eth, nil}
	gpoParams := config.GPO
	if gpoParams.Default == nil {
		gpoParams.Default = config.GasPrice
	}
	eth.ApiBackend.gpo = gasprice.NewOracle(eth.ApiBackend, gpoParams)

	// Set global ipc endpoint.
	eth.blockchain.IPCEndpoint = ctx.GetConfig().IPCEndpoint()

	if eth.chainConfig.Posv != nil {
		c := eth.engine.(*posv.Posv)
		signHook := func(block *types.Block) error {
			eb, err := eth.Etherbase()
			if err != nil {
				log.Error("Cannot get etherbase for append m2 header", "err", err)
				return fmt.Errorf("etherbase missing: %v", err)
			}
			ok := eth.txPool.IsSigner != nil && eth.txPool.IsSigner(eb)
			if !ok {
				return nil
			}
			if block.NumberU64()%common.MergeSignRange == 0 || !eth.chainConfig.IsTIP2019(block.Number()) {
				if err := contracts.CreateTransactionSign(chainConfig, eth.txPool, eth.accountManager, block, chainDb, eb); err != nil {
					return fmt.Errorf("Fail to create tx sign for importing block: %v", err)
				}
			}
			return nil
		}

		appendM2HeaderHook := func(block *types.Block) (*types.Block, bool, error) {
			eb, err := eth.Etherbase()
			if err != nil {
				log.Error("Cannot get etherbase for append m2 header", "err", err)
				return block, false, fmt.Errorf("etherbase missing: %v", err)
			}
			m1, err := c.RecoverSigner(block.Header())
			if err != nil {
				return block, false, fmt.Errorf("can't get block creator: %v", err)
			}
			m2, err := c.GetValidator(m1, eth.blockchain, block.Header())
			if err != nil {
				return block, false, fmt.Errorf("can't get block validator: %v", err)
			}
			if m2 == eb {
				wallet, err := eth.accountManager.Find(accounts.Account{Address: eb})
				if err != nil {
					log.Error("Can't find coinbase account wallet", "err", err)
					return block, false, err
				}
				header := block.Header()
				sighash, err := wallet.SignHash(accounts.Account{Address: eb}, posv.SigHash(header).Bytes())
				if err != nil || sighash == nil {
					log.Error("Can't get signature hash of m2", "sighash", sighash, "err", err)
					return block, false, err
				}
				header.Validator = sighash
				return types.NewBlockWithHeader(header).WithBody(block.Transactions(), block.Uncles()), true, nil
			}
			return block, false, nil
		}

		eth.protocolManager.fetcher.SetSignHook(signHook)
		eth.protocolManager.fetcher.SetAppendM2HeaderHook(appendM2HeaderHook)

		// Hook prepares validators M2 for the current epoch at checkpoint block
		c.HookValidator = func(header *types.Header, signers []common.Address) ([]byte, error) {
			start := time.Now()
			validators, err := GetValidators(eth.blockchain, signers)
			if err != nil {
				return []byte{}, err
			}
			header.Validators = validators
			log.Debug("Time Calculated HookValidator ", "block", header.Number.Uint64(), "time", common.PrettyDuration(time.Since(start)))
			return validators, nil
		}

		// Hook scans for bad masternodes and decide to penalty them
		c.HookPenalty = func(chain consensus.ChainReader, blockNumberEpoc uint64) ([]common.Address, error) {
			canonicalState, err := eth.blockchain.State()
			if canonicalState == nil || err != nil {
				log.Crit("Can't get state at head of canonical chain", "head number", eth.blockchain.CurrentHeader().Number.Uint64(), "err", err)
			}
			prevEpoc := blockNumberEpoc - chain.Config().Posv.Epoch
			if prevEpoc >= 0 {
				start := time.Now()
				prevHeader := chain.GetHeaderByNumber(prevEpoc)
				penSigners := c.GetMasternodes(chain, prevHeader)
				if len(penSigners) > 0 {
					// Loop for each block to check missing sign.
					for i := prevEpoc; i < blockNumberEpoc; i++ {
						if i%common.MergeSignRange == 0 || !chainConfig.IsTIP2019(big.NewInt(int64(i))) {
							bheader := chain.GetHeaderByNumber(i)
							bhash := bheader.Hash()
							block := chain.GetBlock(bhash, i)
							if len(penSigners) > 0 {
								signedMasternodes, err := contracts.GetSignersFromContract(canonicalState, block)
								if err != nil {
									return nil, err
								}
								if len(signedMasternodes) > 0 {
									// Check signer signed?
									for _, signed := range signedMasternodes {
										for j, addr := range penSigners {
											if signed == addr {
												// Remove it from dupSigners.
												penSigners = append(penSigners[:j], penSigners[j+1:]...)
											}
										}
									}
								}
							} else {
								break
							}
						}
					}
				}
				log.Debug("Time Calculated HookPenalty ", "block", blockNumberEpoc, "time", common.PrettyDuration(time.Since(start)))
				return penSigners, nil
			}
			return []common.Address{}, nil
		}

		// Hook scans for bad masternodes and decide to penalty them
		c.HookPenaltyTIPSigning = func(chain consensus.ChainReader, header *types.Header, candidates []common.Address) ([]common.Address, error) {
			prevEpoc := header.Number.Uint64() - chain.Config().Posv.Epoch
			combackEpoch := uint64(0)
			comebackLength := (common.LimitPenaltyEpoch + 1) * chain.Config().Posv.Epoch
			if header.Number.Uint64() > comebackLength {
				combackEpoch = header.Number.Uint64() - comebackLength
			}
			if prevEpoc >= 0 {
				start := time.Now()

				listBlockHash := make([]common.Hash, chain.Config().Posv.Epoch)

				// get list block hash & stats total created block
				statMiners := make(map[common.Address]int)
				listBlockHash[0] = header.ParentHash
				parentnumber := header.Number.Uint64() - 1
				parentHash := header.ParentHash
				for i := uint64(1); i < chain.Config().Posv.Epoch; i++ {
					parentHeader := chain.GetHeader(parentHash, parentnumber)
					miner, _ := c.RecoverSigner(parentHeader)
					value, exist := statMiners[miner]
					if exist {
						value = value + 1
					} else {
						value = 1
					}
					statMiners[miner] = value
					parentHash = parentHeader.ParentHash
					parentnumber--
					listBlockHash[i] = parentHash
				}

				// add list not miner to penalties
				prevHeader := chain.GetHeaderByNumber(prevEpoc)
				preMasternodes := c.GetMasternodes(chain, prevHeader)
				penalties := []common.Address{}
				for miner, total := range statMiners {
					if total < common.MinimunMinerBlockPerEpoch {
						log.Debug("Find a node not enough requirement create block", "addr", miner.Hex(), "total", total)
						penalties = append(penalties, miner)
					}
				}
				for _, addr := range preMasternodes {
					if _, exist := statMiners[addr]; !exist {
						log.Debug("Find a node don't create block", "addr", addr.Hex())
						penalties = append(penalties, addr)
					}
				}

				// get list check penalties signing block & list master nodes wil comeback
				penComebacks := []common.Address{}
				if combackEpoch > 0 {
					combackHeader := chain.GetHeaderByNumber(combackEpoch)
					penalties := common.ExtractAddressFromBytes(combackHeader.Penalties)
					for _, penaltie := range penalties {
						for _, addr := range candidates {
							if penaltie == addr {
								penComebacks = append(penComebacks, penaltie)
							}
						}
					}
				}

				// Loop for each block to check missing sign. with comeback nodes
				mapBlockHash := map[common.Hash]bool{}
				for i := common.RangeReturnSigner - 1; i >= 0; i-- {
					if len(penComebacks) > 0 {
						blockNumber := header.Number.Uint64() - uint64(i) - 1
						bhash := listBlockHash[i]
						if blockNumber%common.MergeSignRange == 0 {
							mapBlockHash[bhash] = true
						}
						signData, ok := c.BlockSigners.Get(bhash)
						if !ok {
							block := chain.GetBlock(bhash, blockNumber)
							txs := block.Transactions()
							signData = c.CacheSigner(bhash, txs)
						}
						txs := signData.([]*types.Transaction)
						// Check signer signed?
						for _, tx := range txs {
							blkHash := common.BytesToHash(tx.Data()[len(tx.Data())-32:])
							from := *tx.From()
							if mapBlockHash[blkHash] {
								for j, addr := range penComebacks {
									if from == addr {
										// Remove it from dupSigners.
										penComebacks = append(penComebacks[:j], penComebacks[j+1:]...)
										break
									}
								}
							}
						}
					} else {
						break
					}
				}

				log.Debug("Time Calculated HookPenaltyTIPSigning ", "block", header.Number, "hash", header.Hash().Hex(), "pen comeback nodes", len(penComebacks), "not enough miner", len(penalties), "time", common.PrettyDuration(time.Since(start)))
				penalties = append(penalties, penComebacks...)
				if chain.Config().IsTIPRandomize(header.Number) {
					return penalties, nil
				}
				return penComebacks, nil
			}
			return []common.Address{}, nil
		}

		/*
		   HookGetSignersFromContract return list masternode for current state (block)
		   This is a solution for work around issue return wrong list signers from snapshot
		*/
		c.HookGetSignersFromContract = func(block common.Hash) ([]common.Address, error) {
			client, err := eth.blockchain.GetClient()
			if err != nil {
				return nil, err
			}
			addr := common.HexToAddress(common.MasternodeVotingSMC)
			validator, err := contractValidator.NewTomoValidator(addr, client)
			if err != nil {
				return nil, err
			}
			opts := new(bind.CallOpts)
			var (
				candidateAddresses []common.Address
				candidates         []posv.Masternode
			)

			stateDB, err := eth.blockchain.StateAt(eth.blockchain.GetBlockByHash(block).Root())
			candidateAddresses = state.GetCandidates(stateDB)

			if err != nil {
				return nil, err
			}
			for _, address := range candidateAddresses {
				v, err := validator.GetCandidateCap(opts, address)
				if err != nil {
					return nil, err
				}
				if address.String() != "0x0000000000000000000000000000000000000000" {
					candidates = append(candidates, posv.Masternode{Address: address, Stake: v})
				}
			}
			// sort candidates by stake descending
			sort.Slice(candidates, func(i, j int) bool {
				return candidates[i].Stake.Cmp(candidates[j].Stake) >= 0
			})
			if len(candidates) > 150 {
				candidates = candidates[:150]
			}
			result := []common.Address{}
			for _, candidate := range candidates {
				result = append(result, candidate.Address)
			}
			return result, nil
		}

		// Hook calculates reward for masternodes
		c.HookReward = func(chain consensus.ChainReader, stateBlock *state.StateDB, parentState *state.StateDB, header *types.Header) (error, map[string]interface{}) {
			number := header.Number.Uint64()
			rCheckpoint := chain.Config().Posv.RewardCheckpoint
			foundationWalletAddr := chain.Config().Posv.FoudationWalletAddr
			if foundationWalletAddr == (common.Address{}) {
				log.Error("Foundation Wallet Address is empty", "error", foundationWalletAddr)
				return errors.New("Foundation Wallet Address is empty"), nil
			}
			rewards := make(map[string]interface{})
			if number > 0 && number-rCheckpoint > 0 && foundationWalletAddr != (common.Address{}) {
				start := time.Now()
				// Get signers in blockSigner smartcontract.
				// Get reward inflation.
				chainReward := new(big.Int).Mul(new(big.Int).SetUint64(chain.Config().Posv.Reward), new(big.Int).SetUint64(params.Ether))
				chainReward = rewardInflation(chainReward, number, common.BlocksPerYear)

				totalSigner := new(uint64)
				signers, err := contracts.GetRewardForCheckpoint(c, chain, header, rCheckpoint, totalSigner)

				log.Debug("Time Get Signers", "block", header.Number.Uint64(), "time", common.PrettyDuration(time.Since(start)))
				if err != nil {
					log.Crit("Fail to get signers for reward checkpoint", "error", err)
				}
				rewards["signers"] = signers
				rewardSigners, err := contracts.CalculateRewardForSigner(chainReward, signers, *totalSigner)
				if err != nil {
					log.Crit("Fail to calculate reward for signers", "error", err)
				}
				// Add reward for coin holders.
				voterResults := make(map[common.Address]interface{})
				if len(signers) > 0 {
					for signer, calcReward := range rewardSigners {
						err, rewards := contracts.CalculateRewardForHolders(foundationWalletAddr, parentState, signer, calcReward, number)
						if err != nil {
							log.Crit("Fail to calculate reward for holders.", "error", err)
						}
						if len(rewards) > 0 {
							for holder, reward := range rewards {
								stateBlock.AddBalance(holder, reward)
							}
						}
						voterResults[signer] = rewards
					}
				}
				rewards["rewards"] = voterResults
				log.Debug("Time Calculated HookReward ", "block", header.Number.Uint64(), "time", common.PrettyDuration(time.Since(start)))
			}
			return nil, rewards
		}

		// Hook verifies masternodes set
		c.HookVerifyMNs = func(header *types.Header, signers []common.Address) error {
			number := header.Number.Int64()
			if number > 0 && number%common.EpocBlockRandomize == 0 {
				start := time.Now()
				validators, err := GetValidators(eth.blockchain, signers)
				log.Debug("Time Calculated HookVerifyMNs ", "block", header.Number.Uint64(), "time", common.PrettyDuration(time.Since(start)))
				if err != nil {
					return err
				}
				if !bytes.Equal(header.Validators, validators) {
					return posv.ErrInvalidCheckpointValidators
				}
			}
			return nil
		}

		eth.txPool.IsSigner = func(address common.Address) bool {
			currentHeader := eth.blockchain.CurrentHeader()
			header := currentHeader
			// Sometimes, the latest block hasn't been inserted to chain yet
			// getSnapshot from parent block if it exists
			parentHeader := eth.blockchain.GetHeader(currentHeader.ParentHash, currentHeader.Number.Uint64()-1)
			if parentHeader != nil {
				// not genesis block
				header = parentHeader
			}
			snap, err := c.GetSnapshot(eth.blockchain, header)
			if err != nil {
				log.Error("Can't get snapshot with at ", "number", header.Number, "hash", header.Hash().Hex(), "err", err)
				return false
			}
			if _, ok := snap.Signers[address]; ok {
				return true
			}
			return false
		}

	}
	return eth, nil
}

func makeExtraData(extra []byte) []byte {
	if len(extra) == 0 {
		// create default extradata
		extra, _ = rlp.EncodeToBytes([]interface{}{
			uint(params.VersionMajor<<16 | params.VersionMinor<<8 | params.VersionPatch),
			"tomo",
			runtime.Version(),
			runtime.GOOS,
		})
	}
	if uint64(len(extra)) > params.MaximumExtraDataSize {
		log.Warn("Miner extra data exceed limit", "extra", hexutil.Bytes(extra), "limit", params.MaximumExtraDataSize)
		extra = nil
	}
	return extra
}

// CreateDB creates the chain database.
func CreateDB(ctx *node.ServiceContext, config *Config, name string) (ethdb.Database, error) {
	db, err := ctx.OpenDatabase(name, config.DatabaseCache, config.DatabaseHandles)
	if err != nil {
		return nil, err
	}
	return db, nil
}

// CreateConsensusEngine creates the required type of consensus engine instance for an Ethereum service
func CreateConsensusEngine(ctx *node.ServiceContext, config *ethash.Config, chainConfig *params.ChainConfig, db ethdb.Database) consensus.Engine {
	// If proof-of-stake-voting is requested, set it up
	if chainConfig.Posv != nil {
		return posv.New(chainConfig.Posv, db)
	}

	// Otherwise assume proof-of-work
	switch {
	case config.PowMode == ethash.ModeFake:
		log.Warn("Ethash used in fake mode")
		return ethash.NewFaker()
	case config.PowMode == ethash.ModeTest:
		log.Warn("Ethash used in test mode")
		return ethash.NewTester()
	case config.PowMode == ethash.ModeShared:
		log.Warn("Ethash used in shared mode")
		return ethash.NewShared()
	default:
		engine := ethash.New(ethash.Config{
			CacheDir:       ctx.ResolvePath(config.CacheDir),
			CachesInMem:    config.CachesInMem,
			CachesOnDisk:   config.CachesOnDisk,
			DatasetDir:     config.DatasetDir,
			DatasetsInMem:  config.DatasetsInMem,
			DatasetsOnDisk: config.DatasetsOnDisk,
		})
		engine.SetThreads(-1) // Disable CPU mining
		return engine
	}
}

// APIs returns the collection of RPC services the ethereum package offers.
// NOTE, some of these services probably need to be moved to somewhere else.
func (s *Ethereum) APIs() []rpc.API {
	apis := ethapi.GetAPIs(s.ApiBackend)

	// Append any APIs exposed explicitly by the consensus engine
	apis = append(apis, s.engine.APIs(s.BlockChain())...)

	// Append all the local APIs and return
	return append(apis, []rpc.API{
		{
			Namespace: "eth",
			Version:   "1.0",
			Service:   NewPublicEthereumAPI(s),
			Public:    true,
		}, {
			Namespace: "eth",
			Version:   "1.0",
			Service:   NewPublicMinerAPI(s),
			Public:    true,
		}, {
			Namespace: "eth",
			Version:   "1.0",
			Service:   downloader.NewPublicDownloaderAPI(s.protocolManager.downloader, s.eventMux),
			Public:    true,
		}, {
			Namespace: "miner",
			Version:   "1.0",
			Service:   NewPrivateMinerAPI(s),
			Public:    false,
		}, {
			Namespace: "eth",
			Version:   "1.0",
			Service:   filters.NewPublicFilterAPI(s.ApiBackend, false),
			Public:    true,
		}, {
			Namespace: "admin",
			Version:   "1.0",
			Service:   NewPrivateAdminAPI(s),
		}, {
			Namespace: "debug",
			Version:   "1.0",
			Service:   NewPublicDebugAPI(s),
			Public:    true,
		}, {
			Namespace: "debug",
			Version:   "1.0",
			Service:   NewPrivateDebugAPI(s.chainConfig, s),
		}, {
			Namespace: "net",
			Version:   "1.0",
			Service:   s.netRPCService,
			Public:    true,
		},
	}...)
}

func (s *Ethereum) ResetWithGenesisBlock(gb *types.Block) {
	s.blockchain.ResetWithGenesisBlock(gb)
}

func (s *Ethereum) Etherbase() (eb common.Address, err error) {
	s.lock.RLock()
	etherbase := s.etherbase
	s.lock.RUnlock()

	if etherbase != (common.Address{}) {
		return etherbase, nil
	}
	if wallets := s.AccountManager().Wallets(); len(wallets) > 0 {
		if accounts := wallets[0].Accounts(); len(accounts) > 0 {
			etherbase := accounts[0].Address

			s.lock.Lock()
			s.etherbase = etherbase
			s.lock.Unlock()

			log.Info("Etherbase automatically configured", "address", etherbase)
			return etherbase, nil
		}
	}
	return common.Address{}, fmt.Errorf("etherbase must be explicitly specified")
}

// set in js console via admin interface or wrapper from cli flags
func (self *Ethereum) SetEtherbase(etherbase common.Address) {
	self.lock.Lock()
	self.etherbase = etherbase
	self.lock.Unlock()

	self.miner.SetEtherbase(etherbase)
}

// ValidateMasternode checks if node's address is in set of masternodes
func (s *Ethereum) ValidateMasternode() (bool, error) {
	eb, err := s.Etherbase()
	if err != nil {
		return false, err
	}
	if s.chainConfig.Posv != nil {
		//check if miner's wallet is in set of validators
		c := s.engine.(*posv.Posv)
		snap, err := c.GetSnapshot(s.blockchain, s.blockchain.CurrentHeader())
		if err != nil {
			return false, fmt.Errorf("Can't verify masternode permission: %v", err)
		}
		if _, authorized := snap.Signers[eb]; !authorized {
			//This miner doesn't belong to set of validators
			return false, nil
		}
	} else {
		return false, fmt.Errorf("Only verify masternode permission in PoSV protocol")
	}
	return true, nil
}

// ValidateMasternodeTestNet checks if node's address is in set of masternodes in Testnet
func (s *Ethereum) ValidateMasternodeTestnet() (bool, error) {
	eb, err := s.Etherbase()
	if err != nil {
		return false, err
	}
	if s.chainConfig.Posv == nil {
		return false, fmt.Errorf("Only verify masternode permission in PoSV protocol")
	}
	masternodes := []common.Address{
		common.HexToAddress("0xfFC679Dcdf444D2eEb0491A998E7902B411CcF20"),
		common.HexToAddress("0xd76fd76F7101811726DCE9E43C2617706a4c45c8"),
		common.HexToAddress("0x8A97753311aeAFACfd76a68Cf2e2a9808d3e65E8"),
	}
	for _, m := range masternodes {
		if m == eb {
			return true, nil
		}
	}
	return false, nil
}

func (s *Ethereum) StartStaking(local bool) error {
	eb, err := s.Etherbase()
	if err != nil {
		log.Error("Cannot start mining without etherbase", "err", err)
		return fmt.Errorf("etherbase missing: %v", err)
	}
	if posv, ok := s.engine.(*posv.Posv); ok {
		wallet, err := s.accountManager.Find(accounts.Account{Address: eb})
		if wallet == nil || err != nil {
			log.Error("Etherbase account unavailable locally", "err", err)
			return fmt.Errorf("signer missing: %v", err)
		}
		posv.Authorize(eb, wallet.SignHash)
	}
	if local {
		// If local (CPU) mining is started, we can disable the transaction rejection
		// mechanism introduced to speed sync times. CPU mining on mainnet is ludicrous
		// so noone will ever hit this path, whereas marking sync done on CPU mining
		// will ensure that private networks work in single miner mode too.
		atomic.StoreUint32(&s.protocolManager.acceptTxs, 1)
	}
	go s.miner.Start(eb)
	return nil
}

func (s *Ethereum) StopStaking() {
	s.miner.Stop()
}
func (s *Ethereum) IsStaking() bool     { return s.miner.Mining() }
func (s *Ethereum) Miner() *miner.Miner { return s.miner }

func (s *Ethereum) AccountManager() *accounts.Manager  { return s.accountManager }
func (s *Ethereum) BlockChain() *core.BlockChain       { return s.blockchain }
func (s *Ethereum) TxPool() *core.TxPool               { return s.txPool }
func (s *Ethereum) EventMux() *event.TypeMux           { return s.eventMux }
func (s *Ethereum) Engine() consensus.Engine           { return s.engine }
func (s *Ethereum) ChainDb() ethdb.Database            { return s.chainDb }
func (s *Ethereum) IsListening() bool                  { return true } // Always listening
func (s *Ethereum) EthVersion() int                    { return int(s.protocolManager.SubProtocols[0].Version) }
func (s *Ethereum) NetVersion() uint64                 { return s.networkId }
func (s *Ethereum) Downloader() *downloader.Downloader { return s.protocolManager.downloader }

// Protocols implements node.Service, returning all the currently configured
// network protocols to start.
func (s *Ethereum) Protocols() []p2p.Protocol {
	if s.lesServer == nil {
		return s.protocolManager.SubProtocols
	}
	return append(s.protocolManager.SubProtocols, s.lesServer.Protocols()...)
}

// Start implements node.Service, starting all internal goroutines needed by the
// Ethereum protocol implementation.
func (s *Ethereum) Start(srvr *p2p.Server) error {
	// Start the bloom bits servicing goroutines
	s.startBloomHandlers()

	// Start the RPC service
	s.netRPCService = ethapi.NewPublicNetAPI(srvr, s.NetVersion())

	// Figure out a max peers count based on the server limits
	maxPeers := srvr.MaxPeers
	if s.config.LightServ > 0 {
		if s.config.LightPeers >= srvr.MaxPeers {
			return fmt.Errorf("invalid peer config: light peer count (%d) >= total peer count (%d)", s.config.LightPeers, srvr.MaxPeers)
		}
		maxPeers -= s.config.LightPeers
	}
	// Start the networking layer and the light server if requested
	s.protocolManager.Start(maxPeers)
	if s.lesServer != nil {
		s.lesServer.Start(srvr)
	}
	return nil
}
func (s *Ethereum) SaveData() {
	s.blockchain.SaveData()
}

// Stop implements node.Service, terminating all internal goroutines used by the
// Ethereum protocol.
func (s *Ethereum) Stop() error {
	s.bloomIndexer.Close()
	s.blockchain.Stop()
	s.protocolManager.Stop()
	if s.lesServer != nil {
		s.lesServer.Stop()
	}
	s.txPool.Stop()
	s.miner.Stop()
	s.eventMux.Stop()

	s.chainDb.Close()
	close(s.shutdownChan)

	return nil
}

func GetValidators(bc *core.BlockChain, masternodes []common.Address) ([]byte, error) {
	if bc.Config().Posv == nil {
		return nil, core.ErrNotPoSV
	}
	client, err := bc.GetClient()
	if err != nil {
		return nil, err
	}
	// Check m2 exists on chaindb.
	// Get secrets and opening at epoc block checkpoint.

	var candidates []int64
	if err != nil {
		return nil, err
	}
	lenSigners := int64(len(masternodes))
	if lenSigners > 0 {
		for _, addr := range masternodes {
			random, err := contracts.GetRandomizeFromContract(client, addr)
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, random)
		}
		// Get randomize m2 list.
		m2, err := contracts.GenM2FromRandomize(candidates, lenSigners)
		if err != nil {
			return nil, err
		}
		return contracts.BuildValidatorFromM2(m2), nil
	}
	return nil, core.ErrNotFoundM1
}

func rewardInflation(chainReward *big.Int, number uint64, blockPerYear uint64) *big.Int {
	if blockPerYear*2 <= number && number < blockPerYear*5 {
		chainReward.Div(chainReward, new(big.Int).SetUint64(2))
	}
	if blockPerYear*5 <= number {
		chainReward.Div(chainReward, new(big.Int).SetUint64(4))
	}

	return chainReward
}

func (s *Ethereum) GetPeer() int {
	return len(s.protocolManager.peers.peers)
}

func (s *Ethereum) GetTomoX() *tomox.TomoX {
	return s.TomoX
}

func (s *Ethereum) OrderPool() *core.OrderPool {
	return s.orderPool
}

func (s *Ethereum) GetTomoXLending() *tomoxlending.Lending {
	return s.Lending
}

// LendingPool geth eth lending pool
func (s *Ethereum) LendingPool() *core.LendingPool {
	return s.lendingPool
}
