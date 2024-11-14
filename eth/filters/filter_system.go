// Copyright 2015 The go-ethereum Authors
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

// Package filters implements an ethereum filtering system for block,
// transactions and log events.
package filters

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"bsc-node/core"
	"bsc-node/core/bloombits"
	"bsc-node/core/types"
	"bsc-node/params"
	"bsc-node/rpc"

	"bsc-node/ethdb"

	"bsc-node/log"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/event"
	// "github.com/ethereum/go-ethereum/params"
	// "github.com/ethereum/go-ethereum/rpc"
)

// Config represents the configuration of the filter system.
type Config struct {
	LogCacheSize int           // maximum number of cached blocks (default: 32)
	Timeout      time.Duration // how long filters stay active (default: 5min)
}

func (cfg Config) withDefaults() Config {
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Minute
	}
	if cfg.LogCacheSize == 0 {
		cfg.LogCacheSize = 32
	}
	return cfg
}

type Backend interface {
	ChainDb() ethdb.Database
	HeaderByNumber(ctx context.Context, blockNr rpc.BlockNumber) (*types.Header, error)
	HeaderByHash(ctx context.Context, blockHash common.Hash) (*types.Header, error)
	GetBody(ctx context.Context, hash common.Hash, number rpc.BlockNumber) (*types.Body, error)
	GetReceipts(ctx context.Context, blockHash common.Hash) (types.Receipts, error)
	GetLogs(ctx context.Context, blockHash common.Hash, number uint64) ([][]*types.Log, error)
	// PendingBlockAndReceipts() (*types.Block, types.Receipts)

	CurrentHeader() *types.Header
	ChainConfig() *params.ChainConfig
	SubscribeNewTxsEvent(chan<- core.NewTxsEvent) event.Subscription
	SubscribeChainEvent(ch chan<- core.ChainEvent) event.Subscription
	SubscribeFinalizedHeaderEvent(ch chan<- core.FinalizedHeaderEvent) event.Subscription
	SubscribeRemovedLogsEvent(ch chan<- core.RemovedLogsEvent) event.Subscription
	SubscribeLogsEvent(ch chan<- []*types.Log) event.Subscription
	// SubscribePendingLogsEvent(ch chan<- []*types.Log) event.Subscription
	SubscribeNewVoteEvent(chan<- core.NewVoteEvent) event.Subscription

	BloomStatus() (uint64, uint64)
	ServiceFilter(ctx context.Context, session *bloombits.MatcherSession)
}

// FilterSystem holds resources shared by all filters.
type FilterSystem struct {
	backend   Backend
	logsCache *lru.Cache[common.Hash, *logCacheElem]
	cfg       *Config
}

// NewFilterSystem creates a filter system.
func NewFilterSystem(backend Backend, config Config) *FilterSystem {
	config = config.withDefaults()
	return &FilterSystem{
		backend:   backend,
		logsCache: lru.NewCache[common.Hash, *logCacheElem](config.LogCacheSize),
		cfg:       &config,
	}
}

type logCacheElem struct {
	logs []*types.Log
	body atomic.Value
}

// cachedLogElem loads block logs from the backend and caches the result.
func (sys *FilterSystem) cachedLogElem(ctx context.Context, blockHash common.Hash, number uint64) (*logCacheElem, error) {
	cached, ok := sys.logsCache.Get(blockHash)
	if ok {
		return cached, nil
	}

	logs, err := sys.backend.GetLogs(ctx, blockHash, number)
	if err != nil {
		return nil, err
	}
	if logs == nil {
		return nil, fmt.Errorf("failed to get logs for block #%d (0x%s)", number, blockHash.TerminalString())
	}
	// Database logs are un-derived.
	// Fill in whatever we can (txHash is inaccessible at this point).
	flattened := make([]*types.Log, 0)
	var logIdx uint
	for i, txLogs := range logs {
		for _, log := range txLogs {
			log.BlockHash = blockHash
			log.BlockNumber = number
			log.TxIndex = uint(i)
			log.Index = logIdx
			logIdx++
			flattened = append(flattened, log)
		}
	}
	elem := &logCacheElem{logs: flattened}
	sys.logsCache.Add(blockHash, elem)
	return elem, nil
}

func (sys *FilterSystem) cachedGetBody(ctx context.Context, elem *logCacheElem, hash common.Hash, number uint64) (*types.Body, error) {
	if body := elem.body.Load(); body != nil {
		return body.(*types.Body), nil
	}
	body, err := sys.backend.GetBody(ctx, hash, rpc.BlockNumber(number))
	if err != nil {
		return nil, err
	}
	elem.body.Store(body)
	return body, nil
}

// Type determines the kind of filter and is used to put the filter in to
// the correct bucket when added.
type Type byte

const (
	// UnknownSubscription indicates an unknown subscription type
	UnknownSubscription Type = iota
	// LogsSubscription queries for new or removed (chain reorg) logs
	LogsSubscription
	// PendingLogsSubscription queries for logs in pending blocks
	PendingLogsSubscription
	// MinedAndPendingLogsSubscription queries for logs in mined and pending blocks.
	MinedAndPendingLogsSubscription
	// PendingTransactionsSubscription queries for pending transactions entering
	// the pending state
	PendingTransactionsSubscription
	// BlocksSubscription queries hashes for blocks that are imported
	BlocksSubscription
	// VotesSubscription queries vote hashes for votes entering the vote pool
	VotesSubscription
	// FinalizedHeadersSubscription queries hashes for finalized headers that are reached
	FinalizedHeadersSubscription
	// LastIndexSubscription keeps track of the last index
	LastIndexSubscription
)

const (
	// txChanSize is the size of channel listening to NewTxsEvent.
	// The number is referenced from the size of tx pool.
	txChanSize = 4096
	// rmLogsChanSize is the size of channel listening to RemovedLogsEvent.
	rmLogsChanSize = 10
	// logsChanSize is the size of channel listening to LogsEvent.
	logsChanSize = 10
	// chainEvChanSize is the size of channel listening to ChainEvent.
	chainEvChanSize = 10
	// finalizedHeaderEvChanSize is the size of channel listening to FinalizedHeaderEvent.
	finalizedHeaderEvChanSize = 10
	// voteChanSize is the size of channel listening to NewVoteEvent.
	// The number is referenced from the size of vote pool.
	voteChanSize = 256
)

type subscription struct {
	id        rpc.ID
	typ       Type
	created   time.Time
	logsCrit  ethereum.FilterQuery
	logs      chan []*types.Log
	txs       chan []*types.Transaction
	headers   chan *types.Header
	votes     chan *types.VoteEnvelope
	installed chan struct{} // closed when the filter is installed
	err       chan error    // closed when the filter is uninstalled
}

// EventSystem creates subscriptions, processes events and broadcasts them to the
// subscription which match the subscription criteria.
type EventSystem struct {
	backend Backend
	sys     *FilterSystem

	// Subscriptions
	txsSub             event.Subscription // Subscription for new transaction event
	logsSub            event.Subscription // Subscription for new log event
	rmLogsSub          event.Subscription // Subscription for removed log event
	pendingLogsSub     event.Subscription // Subscription for pending log event
	chainSub           event.Subscription // Subscription for new chain event
	finalizedHeaderSub event.Subscription // Subscription for new finalized header
	voteSub            event.Subscription // Subscription for new vote event

	// Channels
	install           chan *subscription             // install filter for event notification
	uninstall         chan *subscription             // remove filter for event notification
	txsCh             chan core.NewTxsEvent          // Channel to receive new transactions event
	logsCh            chan []*types.Log              // Channel to receive new log event
	pendingLogsCh     chan []*types.Log              // Channel to receive new log event
	rmLogsCh          chan core.RemovedLogsEvent     // Channel to receive removed log event
	chainCh           chan core.ChainEvent           // Channel to receive new chain event
	finalizedHeaderCh chan core.FinalizedHeaderEvent // Channel to receive new finalized header event
	voteCh            chan core.NewVoteEvent         // Channel to receive new vote event
}

// NewEventSystem creates a new manager that listens for event on the given mux,
// parses and filters them. It uses the all map to retrieve filter changes. The
// work loop holds its own index that is used to forward events to filters.
//
// The returned manager has a loop that needs to be stopped with the Stop function
// or by stopping the given mux.
func NewEventSystem(sys *FilterSystem) *EventSystem {
	m := &EventSystem{
		sys:               sys,
		backend:           sys.backend,
		install:           make(chan *subscription),
		uninstall:         make(chan *subscription),
		txsCh:             make(chan core.NewTxsEvent, txChanSize),
		logsCh:            make(chan []*types.Log, logsChanSize),
		rmLogsCh:          make(chan core.RemovedLogsEvent, rmLogsChanSize),
		pendingLogsCh:     make(chan []*types.Log, logsChanSize),
		chainCh:           make(chan core.ChainEvent, chainEvChanSize),
		finalizedHeaderCh: make(chan core.FinalizedHeaderEvent, finalizedHeaderEvChanSize),
		voteCh:            make(chan core.NewVoteEvent, voteChanSize),
	}

	// Subscribe events
	m.txsSub = m.backend.SubscribeNewTxsEvent(m.txsCh)
	m.logsSub = m.backend.SubscribeLogsEvent(m.logsCh)
	m.rmLogsSub = m.backend.SubscribeRemovedLogsEvent(m.rmLogsCh)
	m.chainSub = m.backend.SubscribeChainEvent(m.chainCh)
	// m.pendingLogsSub = m.backend.SubscribePendingLogsEvent(m.pendingLogsCh)
	m.finalizedHeaderSub = m.backend.SubscribeFinalizedHeaderEvent(m.finalizedHeaderCh)
	m.voteSub = m.backend.SubscribeNewVoteEvent(m.voteCh)

	// Make sure none of the subscriptions are empty
	if m.txsSub == nil || m.logsSub == nil || m.rmLogsSub == nil || m.chainSub == nil || m.pendingLogsSub == nil {
		log.Crit("Subscribe for event system failed")
	}
	if m.voteSub == nil || m.finalizedHeaderSub == nil {
		log.Warn("Subscribe for vote or finalized header event failed")
	}

	go m.eventLoop()
	return m
}

// Subscription is created when the client registers itself for a particular event.
type Subscription struct {
	ID        rpc.ID
	f         *subscription
	es        *EventSystem
	unsubOnce sync.Once
}

// Err returns a channel that is closed when unsubscribed.
func (sub *Subscription) Err() <-chan error {
	return sub.f.err
}

// Unsubscribe uninstalls the subscription from the event broadcast loop.
func (sub *Subscription) Unsubscribe() {
	sub.unsubOnce.Do(func() {
	uninstallLoop:
		for {
			// write uninstall request and consume logs/hashes. This prevents
			// the eventLoop broadcast method to deadlock when writing to the
			// filter event channel while the subscription loop is waiting for
			// this method to return (and thus not reading these events).
			select {
			case sub.es.uninstall <- sub.f:
				break uninstallLoop
			case <-sub.f.logs:
			case <-sub.f.txs:
			case <-sub.f.headers:
			case <-sub.f.votes:
			}
		}

		// wait for filter to be uninstalled in work loop before returning
		// this ensures that the manager won't use the event channel which
		// will probably be closed by the client asap after this method returns.
		<-sub.Err()
	})
}

// subscribe installs the subscription in the event broadcast loop.
func (es *EventSystem) subscribe(sub *subscription) *Subscription {
	es.install <- sub
	<-sub.installed
	return &Subscription{ID: sub.id, f: sub, es: es}
}

// SubscribeLogs creates a subscription that will write all logs matching the
// given criteria to the given logs channel. Default value for the from and to
// block is "latest". If the fromBlock > toBlock an error is returned.
func (es *EventSystem) SubscribeLogs(crit ethereum.FilterQuery, logs chan []*types.Log) (*Subscription, error) {
	var from, to rpc.BlockNumber
	if crit.FromBlock == nil {
		from = rpc.LatestBlockNumber
	} else {
		from = rpc.BlockNumber(crit.FromBlock.Int64())
	}
	if crit.ToBlock == nil {
		to = rpc.LatestBlockNumber
	} else {
		to = rpc.BlockNumber(crit.ToBlock.Int64())
	}

	// only interested in pending logs
	if from == rpc.PendingBlockNumber && to == rpc.PendingBlockNumber {
		return es.subscribePendingLogs(crit, logs), nil
	}
	// only interested in new mined logs
	if from == rpc.LatestBlockNumber && to == rpc.LatestBlockNumber {
		return es.subscribeLogs(crit, logs), nil
	}
	// only interested in mined logs within a specific block range
	if from >= 0 && to >= 0 && to >= from {
		return es.subscribeLogs(crit, logs), nil
	}
	// interested in mined logs from a specific block number, new logs and pending logs
	if from >= rpc.LatestBlockNumber && to == rpc.PendingBlockNumber {
		return es.subscribeMinedPendingLogs(crit, logs), nil
	}
	// interested in logs from a specific block number to new mined blocks
	if from >= 0 && to == rpc.LatestBlockNumber {
		return es.subscribeLogs(crit, logs), nil
	}
	return nil, errors.New("invalid from and to block combination: from > to")
}

// subscribeMinedPendingLogs creates a subscription that returned mined and
// pending logs that match the given criteria.
func (es *EventSystem) subscribeMinedPendingLogs(crit ethereum.FilterQuery, logs chan []*types.Log) *Subscription {
	sub := &subscription{
		id:        rpc.NewID(),
		typ:       MinedAndPendingLogsSubscription,
		logsCrit:  crit,
		created:   time.Now(),
		logs:      logs,
		txs:       make(chan []*types.Transaction),
		headers:   make(chan *types.Header),
		votes:     make(chan *types.VoteEnvelope),
		installed: make(chan struct{}),
		err:       make(chan error),
	}
	return es.subscribe(sub)
}

// subscribeLogs creates a subscription that will write all logs matching the
// given criteria to the given logs channel.
func (es *EventSystem) subscribeLogs(crit ethereum.FilterQuery, logs chan []*types.Log) *Subscription {
	sub := &subscription{
		id:        rpc.NewID(),
		typ:       LogsSubscription,
		logsCrit:  crit,
		created:   time.Now(),
		logs:      logs,
		txs:       make(chan []*types.Transaction),
		headers:   make(chan *types.Header),
		votes:     make(chan *types.VoteEnvelope),
		installed: make(chan struct{}),
		err:       make(chan error),
	}
	return es.subscribe(sub)
}

// subscribePendingLogs creates a subscription that writes contract event logs for
// transactions that enter the transaction pool.
func (es *EventSystem) subscribePendingLogs(crit ethereum.FilterQuery, logs chan []*types.Log) *Subscription {
	sub := &subscription{
		id:        rpc.NewID(),
		typ:       PendingLogsSubscription,
		logsCrit:  crit,
		created:   time.Now(),
		logs:      logs,
		txs:       make(chan []*types.Transaction),
		headers:   make(chan *types.Header),
		votes:     make(chan *types.VoteEnvelope),
		installed: make(chan struct{}),
		err:       make(chan error),
	}
	return es.subscribe(sub)
}

// SubscribeNewHeads creates a subscription that writes the header of a block that is
// imported in the chain.
func (es *EventSystem) SubscribeNewHeads(headers chan *types.Header) *Subscription {
	sub := &subscription{
		id:        rpc.NewID(),
		typ:       BlocksSubscription,
		created:   time.Now(),
		logs:      make(chan []*types.Log),
		txs:       make(chan []*types.Transaction),
		headers:   headers,
		votes:     make(chan *types.VoteEnvelope),
		installed: make(chan struct{}),
		err:       make(chan error),
	}
	return es.subscribe(sub)
}

// SubscribeNewFinalizedHeaders creates a subscription that writes the finalized header of a block that is
// reached recently.
func (es *EventSystem) SubscribeNewFinalizedHeaders(headers chan *types.Header) *Subscription {
	sub := &subscription{
		id:        rpc.NewID(),
		typ:       FinalizedHeadersSubscription,
		created:   time.Now(),
		logs:      make(chan []*types.Log),
		txs:       make(chan []*types.Transaction),
		headers:   headers,
		votes:     make(chan *types.VoteEnvelope),
		installed: make(chan struct{}),
		err:       make(chan error),
	}
	return es.subscribe(sub)
}

// SubscribePendingTxs creates a subscription that writes transactions for
// transactions that enter the transaction pool.
func (es *EventSystem) SubscribePendingTxs(txs chan []*types.Transaction) *Subscription {
	sub := &subscription{
		id:        rpc.NewID(),
		typ:       PendingTransactionsSubscription,
		created:   time.Now(),
		logs:      make(chan []*types.Log),
		txs:       txs,
		headers:   make(chan *types.Header),
		votes:     make(chan *types.VoteEnvelope),
		installed: make(chan struct{}),
		err:       make(chan error),
	}
	return es.subscribe(sub)
}

// SubscribeNewVotes creates a subscription that writes transaction hashes for
// transactions that enter the transaction pool.
func (es *EventSystem) SubscribeNewVotes(votes chan *types.VoteEnvelope) *Subscription {
	sub := &subscription{
		id:        rpc.NewID(),
		typ:       VotesSubscription,
		created:   time.Now(),
		logs:      make(chan []*types.Log),
		txs:       make(chan []*types.Transaction),
		headers:   make(chan *types.Header),
		votes:     votes,
		installed: make(chan struct{}),
		err:       make(chan error),
	}
	return es.subscribe(sub)
}

type filterIndex map[Type]map[rpc.ID]*subscription

func (es *EventSystem) handleLogs(filters filterIndex, ev []*types.Log) {
	if len(ev) == 0 {
		return
	}
	for _, f := range filters[LogsSubscription] {
		matchedLogs := filterLogs(ev, f.logsCrit.FromBlock, f.logsCrit.ToBlock, f.logsCrit.Addresses, f.logsCrit.Topics)
		if len(matchedLogs) > 0 {
			f.logs <- matchedLogs
		}
	}
}

func (es *EventSystem) handlePendingLogs(filters filterIndex, ev []*types.Log) {
	if len(ev) == 0 {
		return
	}
	for _, f := range filters[PendingLogsSubscription] {
		matchedLogs := filterLogs(ev, nil, f.logsCrit.ToBlock, f.logsCrit.Addresses, f.logsCrit.Topics)
		if len(matchedLogs) > 0 {
			f.logs <- matchedLogs
		}
	}
}

func (es *EventSystem) handleTxsEvent(filters filterIndex, ev core.NewTxsEvent) {
	for _, f := range filters[PendingTransactionsSubscription] {
		f.txs <- ev.Txs
	}
}

func (es *EventSystem) handleVoteEvent(filters filterIndex, ev core.NewVoteEvent) {
	for _, f := range filters[VotesSubscription] {
		f.votes <- ev.Vote
	}
}

func (es *EventSystem) handleChainEvent(filters filterIndex, ev core.ChainEvent) {
	for _, f := range filters[BlocksSubscription] {
		f.headers <- ev.Block.Header()
	}
}

func (es *EventSystem) handleFinalizedHeaderEvent(filters filterIndex, ev core.FinalizedHeaderEvent) {
	for _, f := range filters[FinalizedHeadersSubscription] {
		f.headers <- ev.Header
	}
}

// eventLoop (un)installs filters and processes mux events.
func (es *EventSystem) eventLoop() {
	// Ensure all subscriptions get cleaned up
	defer func() {
		es.txsSub.Unsubscribe()
		es.logsSub.Unsubscribe()
		es.rmLogsSub.Unsubscribe()
		es.pendingLogsSub.Unsubscribe()
		es.chainSub.Unsubscribe()
		es.finalizedHeaderSub.Unsubscribe()
		if es.voteSub != nil {
			es.voteSub.Unsubscribe()
		}
	}()

	index := make(filterIndex)
	for i := UnknownSubscription; i < LastIndexSubscription; i++ {
		index[i] = make(map[rpc.ID]*subscription)
	}

	var voteSubErr <-chan error
	if es.voteSub != nil {
		voteSubErr = es.voteSub.Err()
	}
	for {
		select {
		case ev := <-es.txsCh:
			es.handleTxsEvent(index, ev)
		case ev := <-es.logsCh:
			es.handleLogs(index, ev)
		case ev := <-es.rmLogsCh:
			es.handleLogs(index, ev.Logs)
		case ev := <-es.pendingLogsCh:
			es.handlePendingLogs(index, ev)
		case ev := <-es.chainCh:
			es.handleChainEvent(index, ev)
		case ev := <-es.finalizedHeaderCh:
			es.handleFinalizedHeaderEvent(index, ev)
		case ev := <-es.voteCh:
			es.handleVoteEvent(index, ev)

		case f := <-es.install:
			if f.typ == MinedAndPendingLogsSubscription {
				// the type are logs and pending logs subscriptions
				index[LogsSubscription][f.id] = f
				index[PendingLogsSubscription][f.id] = f
			} else {
				index[f.typ][f.id] = f
			}
			close(f.installed)

		case f := <-es.uninstall:
			if f.typ == MinedAndPendingLogsSubscription {
				// the type are logs and pending logs subscriptions
				delete(index[LogsSubscription], f.id)
				delete(index[PendingLogsSubscription], f.id)
			} else {
				delete(index[f.typ], f.id)
			}
			close(f.err)

		// System stopped
		case <-es.txsSub.Err():
			return
		case <-es.logsSub.Err():
			return
		case <-es.rmLogsSub.Err():
			return
		case <-es.chainSub.Err():
			return
		case <-es.finalizedHeaderSub.Err():
			return
		case <-voteSubErr:
			return
		}
	}
}
