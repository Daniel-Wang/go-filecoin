package node

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math/big"
	"sync"

	dag "github.com/ipfs/go-ipfs/merkledag"
	cbor "gx/ipfs/QmRVSCwQtW1rjHCay9NqKXDwbtKTgDcN4iY7PrpSqfKM5D/go-ipld-cbor"
	"gx/ipfs/QmVmDhyTTUcQXFD1rRQ64fGLMSAoaQvNH3hwuaCFAPq2hy/errors"
	inet "gx/ipfs/QmXfkENeeBvh3zYA51MaSdGUdBjhQ99cP5WQe8zgr6wchG/go-libp2p-net"
	"gx/ipfs/QmZNkThpqfVXs9GNbexPrfBbXSLNYeKrE7jwFM2oqHbyqN/go-libp2p-protocol"
	"gx/ipfs/QmcZfnkapfECQGcLZaf9B79NRg7cRa9EnZh4LSbkCzwNvY/go-cid"

	"github.com/filecoin-project/go-filecoin/abi"
	cbu "github.com/filecoin-project/go-filecoin/cborutil"
	"github.com/filecoin-project/go-filecoin/core"
	"github.com/filecoin-project/go-filecoin/types"
)

// MakeDealProtocolID is the protocol ID for the make deal protocol
const MakeDealProtocolID = protocol.ID("/fil/deal/mk/1.0.0")

// QueryDealProtocolID is the protocol ID for the query deal protocol
const QueryDealProtocolID = protocol.ID("/fil/deal/qry/1.0.0")

func init() {
	cbor.RegisterCborType(DealProposal{})
	cbor.RegisterCborType(DealResponse{})
	cbor.RegisterCborType(DealQuery{})
}

// DealProposal is used for a storage client to propose a deal. It is up to the
// creator of the proposal to select a bid and an ask and turn that into a
// deal, add a reference to the data they want stored to the deal,  then add
// their signature over the deal.
type DealProposal struct {
	Deal      *core.Deal `json:"deal"`
	ClientSig string     `json:"clientsig"`
}

// DealQuery is used to query the state of a deal by its miner generated ID
type DealQuery struct {
	ID [32]byte `json:"ID"`
}

// DealResponse is returned from the miner after a deal proposal or a deal query
type DealResponse struct {
	// State is the current state of the referenced deal
	State DealState `json:"state"`

	// Message is an informational string to aid in interpreting the State. It
	// should not be relied on for any system logic.
	Message string `json:"message"`

	// MsgCid is the cid of the 'addDeal' message once the deal is accepted and
	// posted to the blockchain
	MsgCid *cid.Cid `json:"msgCid"`

	// ID is an identifying string generated by the miner to track this
	// deal-in-progress
	ID [32]byte `json:"ID"`
}

// StorageMarket manages making storage deals with clients
type StorageMarket struct {
	// TODO: don't depend directly on the node once we find out exactly the set
	// of things we need from it. blah blah passing in function closure nonsense blah blah
	nd *Node

	deals struct {
		set map[[32]byte]*Negotiation
		sync.Mutex
	}

	// smi allows the StorageMarket to fetch data on asks bids and deals from
	// the blockchain (or some mocked source for testing)
	smi storageMarketPeeker
}

// DealState signifies the state of a deal
type DealState int

const (
	// Unknown signifies an unknown negotiation
	Unknown = DealState(iota)

	// Rejected means the deal was rejected for some reason
	Rejected

	// Accepted means the deal was accepted but hasnt yet started
	Accepted

	// Started means the deal has started and the transfer is in progress
	Started

	// Failed means the deal has failed for some reason
	Failed

	// Posted means the deal has been posted to the blockchain
	Posted

	// Complete means the deal is complete
	// TODO: distinguish this from 'Posted'
	Complete
)

func (s DealState) String() string {
	switch s {
	case Unknown:
		return "unknown"
	case Rejected:
		return "rejected"
	case Accepted:
		return "accepted"
	case Started:
		return "started"
	case Failed:
		return "failed"
	case Posted:
		return "posted"
	case Complete:
		return "complete"
	default:
		return fmt.Sprintf("<unrecognized %d>", s)
	}
}

// Negotiation tracks an in-progress deal between a miner and a storage client
type Negotiation struct {
	DealProposal *DealProposal
	MsgCid       *cid.Cid
	State        DealState
	Error        string

	// MinerOwner is the owner of the miner in this deals ask. It is controlled
	// by this nodes operator.
	MinerOwner types.Address
}

// NewStorageMarket sets up a new storage market protocol handler and registers
// it with libp2p
func NewStorageMarket(nd *Node) *StorageMarket {
	sm := &StorageMarket{
		nd:  nd,
		smi: &stateTreeMarketPeeker{nd},
	}
	sm.deals.set = make(map[[32]byte]*Negotiation)

	nd.Host.SetStreamHandler(MakeDealProtocolID, sm.handleNewStreamPropose)
	nd.Host.SetStreamHandler(QueryDealProtocolID, sm.handleNewStreamQuery)

	return sm
}

// ProposeDeal the handler for incoming deal proposals
func (sm *StorageMarket) ProposeDeal(propose *DealProposal) (*DealResponse, error) {
	ask, err := sm.smi.GetAsk(propose.Deal.Ask)
	if err != nil {
		return &DealResponse{
			Message: fmt.Sprintf("unknown ask: %s", err),
			State:   Rejected,
		}, nil
	}

	bid, err := sm.smi.GetBid(propose.Deal.Bid)
	if err != nil {
		return &DealResponse{
			Message: fmt.Sprintf("unknown bid: %s", err),
			State:   Rejected,
		}, nil
	}

	// TODO: also validate that the bids and asks are not expired
	if bid.Used {
		return &DealResponse{
			Message: "bid already used",
			State:   Rejected,
		}, nil
	}

	mowner, err := sm.smi.GetMinerOwner(context.TODO(), ask.Owner)
	if err != nil {
		// TODO: does this get a response? This means that we have an ask whose
		// miner we couldnt look up. Feels like an invariant being invalidated,
		// or a system fault.
		return nil, err
	}

	if !sm.nd.Wallet.HasAddress(mowner) {
		return &DealResponse{
			Message: "ask in deal proposal does not belong to us",
			State:   Rejected,
		}, nil
	}

	if bid.Size.GreaterThan(ask.Size) {
		return &DealResponse{
			Message: "ask does not have enough space for bid",
			State:   Rejected,
		}, nil
	}

	// TODO: validate pairing of bid and ask
	// TODO: ensure bid and ask arent already part of a deal we have accepted

	// TODO: don't always auto accept, we should be able to expose this choice to the user
	// TODO: even under 'auto accept', have some restrictions around minimum
	// price and requested collateral.

	id := negotiationID(mowner, propose)
	sm.deals.Lock()
	defer sm.deals.Unlock()

	oneg, ok := sm.deals.set[id]
	if ok {
		return &DealResponse{
			Message: "deal negotiation already in progress",
			State:   oneg.State,
			ID:      id,
		}, nil
	}

	neg := &Negotiation{
		DealProposal: propose,
		State:        Accepted,
		MinerOwner:   mowner,
	}

	sm.deals.set[id] = neg

	// TODO: put this into a scheduler
	go sm.processDeal(id)

	return &DealResponse{
		State: Accepted,
		ID:    id,
	}, nil
}

func (sm *StorageMarket) updateNegotiation(id [32]byte, op func(*Negotiation)) {
	sm.deals.Lock()
	defer sm.deals.Unlock()

	op(sm.deals.set[id])
}

func (sm *StorageMarket) handleNewStreamPropose(s inet.Stream) {
	defer s.Close() // nolint: errcheck
	r := cbu.NewMsgReader(s)
	w := cbu.NewMsgWriter(s)

	var propose DealProposal
	if err := r.ReadMsg(&propose); err != nil {
		s.Reset() // nolint: errcheck
		log.Warningf("failed to read DealProposal: %s", err)
		return
	}

	resp, err := sm.ProposeDeal(&propose)
	if err != nil {
		s.Reset() // nolint: errcheck
		// TODO: metrics, more structured logging. This is fairly useful information
		log.Infof("incoming deal proposal failed: %s", err)
	}

	if err := w.WriteMsg(resp); err != nil {
		log.Warningf("failed to write back deal propose response: %s", err)
	}
}

func (sm *StorageMarket) handleNewStreamQuery(s inet.Stream) {
	defer s.Close() // nolint: errcheck
	r := cbu.NewMsgReader(s)
	w := cbu.NewMsgWriter(s)

	var q DealQuery
	if err := r.ReadMsg(&q); err != nil {
		s.Reset() // nolint: errcheck
		log.Warningf("failed to read deal query: %s", err)
		return
	}

	resp, err := sm.QueryDeal(q.ID)
	if err != nil {
		s.Reset() // nolint: errcheck
		// TODO: metrics, more structured logging. This is fairly useful information
		log.Infof("incoming deal query failed: %s", err)
	}

	if err := w.WriteMsg(resp); err != nil {
		log.Warningf("failed to write back deal query response: %s", err)
	}
}

// QueryDeal is the handler for incoming deal queries
func (sm *StorageMarket) QueryDeal(id [32]byte) (*DealResponse, error) {
	sm.deals.Lock()
	defer sm.deals.Unlock()

	neg, ok := sm.deals.set[id]
	if !ok {
		return &DealResponse{State: Unknown}, nil
	}

	return &DealResponse{
		State:   neg.State,
		Message: neg.Error,
		MsgCid:  neg.MsgCid,
	}, nil
}

func negotiationID(minerID types.Address, propose *DealProposal) [32]byte {
	data, err := cbor.DumpObject(propose)
	if err != nil {
		panic(err)
	}

	data = append(data, minerID[:]...)

	return sha256.Sum256(data)
}

func (sm *StorageMarket) processDeal(id [32]byte) {
	var propose *DealProposal
	var minerOwner types.Address
	sm.updateNegotiation(id, func(n *Negotiation) {
		propose = n.DealProposal
		minerOwner = n.MinerOwner
		n.State = Started
	})

	msgcid, err := sm.finishDeal(context.TODO(), minerOwner, propose)
	if err != nil {
		log.Warning(err)
		sm.updateNegotiation(id, func(n *Negotiation) {
			n.State = Failed
			n.Error = err.Error()
		})
		return
	}

	sm.updateNegotiation(id, func(n *Negotiation) {
		n.MsgCid = msgcid
		n.State = Posted
	})
}

func (sm *StorageMarket) finishDeal(ctx context.Context, minerOwner types.Address, propose *DealProposal) (*cid.Cid, error) {
	// TODO: better file fetching
	if err := sm.fetchData(context.TODO(), propose.Deal.DataRef); err != nil {
		return nil, errors.Wrap(err, "fetching data failed")
	}

	msgcid, err := sm.smi.AddDeal(ctx, minerOwner, propose.Deal.Ask, propose.Deal.Bid, propose.ClientSig)
	if err != nil {
		return nil, err
	}

	if err := sm.stageForSealing(ctx, propose.Deal.DataRef); err != nil {
		// TODO: maybe wait until the deal gets finalized on the blockchain? (wait N blocks)
		return nil, err
	}

	return msgcid, nil
}

func (sm *StorageMarket) stageForSealing(ctx context.Context, ref *cid.Cid) error {
	// TODO:
	return nil
}

func (sm *StorageMarket) fetchData(ctx context.Context, ref *cid.Cid) error {
	return dag.FetchGraph(ctx, ref, dag.NewDAGService(sm.nd.Blockservice))
}

// GetMarketPeeker returns the storageMarketPeeker for this storage market
// TODO: something something returning unexported interfaces?
func (sm *StorageMarket) GetMarketPeeker() storageMarketPeeker { // nolint: golint
	return sm.smi
}

type storageMarketPeeker interface {
	GetAsk(uint64) (*core.Ask, error)
	GetBid(uint64) (*core.Bid, error)
	AddDeal(ctx context.Context, from types.Address, bid, ask uint64, sig string) (*cid.Cid, error)

	// more of a gape than a peek..
	GetAskSet() (core.AskSet, error)
	GetBidSet() (core.BidSet, error)
	GetDealList() ([]*core.Deal, error)
	GetMinerOwner(context.Context, types.Address) (types.Address, error)
}

type stateTreeMarketPeeker struct {
	nd *Node
}

func (stsa *stateTreeMarketPeeker) loadStateTree(ctx context.Context) (types.StateTree, error) {
	bestBlk := stsa.nd.ChainMgr.GetBestBlock()
	return types.LoadStateTree(ctx, stsa.nd.CborStore, bestBlk.StateRoot)
}

func (stsa *stateTreeMarketPeeker) loadStorageMarketActorStorage(ctx context.Context) (*core.StorageMarketStorage, error) {
	st, err := stsa.loadStateTree(ctx)
	if err != nil {
		return nil, err
	}

	act, err := st.GetActor(ctx, core.StorageMarketAddress)
	if err != nil {
		return nil, err
	}

	var storage core.StorageMarketStorage
	if err := core.UnmarshalStorage(act.ReadStorage(), &storage); err != nil {
		return nil, err
	}

	return &storage, nil
}

// GetAsk returns the given ask from the current state of the storage market actor
func (stsa *stateTreeMarketPeeker) GetAsk(id uint64) (*core.Ask, error) {
	stor, err := stsa.loadStorageMarketActorStorage(context.TODO())
	if err != nil {
		return nil, err
	}

	ask, ok := stor.Orderbook.Asks[id]
	if !ok {
		return nil, fmt.Errorf("no such ask")
	}

	return ask, nil
}

// GetBid returns the given bid from the current state of the storage market actor
func (stsa *stateTreeMarketPeeker) GetBid(id uint64) (*core.Bid, error) {
	stor, err := stsa.loadStorageMarketActorStorage(context.TODO())
	if err != nil {
		return nil, err
	}

	bid, ok := stor.Orderbook.Bids[id]
	if !ok {
		return nil, fmt.Errorf("no such bid")
	}

	return bid, nil
}

// GetAskSet returns the given the entire ask set from the storage market
// TODO limit number of results
func (stsa *stateTreeMarketPeeker) GetAskSet() (core.AskSet, error) {
	stor, err := stsa.loadStorageMarketActorStorage(context.TODO())
	if err != nil {
		return nil, err
	}

	return stor.Orderbook.Asks, nil
}

// GetBidSet returns the given the entire bid set from the storage market
// TODO limit number of results
func (stsa *stateTreeMarketPeeker) GetBidSet() (core.BidSet, error) {
	stor, err := stsa.loadStorageMarketActorStorage(context.TODO())
	if err != nil {
		return nil, err
	}

	return stor.Orderbook.Bids, nil
}

// GetDealList returns the given the entire bid set from the storage market
// TODO limit the number of results
func (stsa *stateTreeMarketPeeker) GetDealList() ([]*core.Deal, error) {
	stor, err := stsa.loadStorageMarketActorStorage(context.TODO())
	if err != nil {
		return nil, err
	}

	return stor.Orderbook.Deals, nil
}

func (stsa *stateTreeMarketPeeker) GetMinerOwner(ctx context.Context, miner types.Address) (types.Address, error) {
	st, err := stsa.loadStateTree(ctx)
	if err != nil {
		return types.Address{}, err
	}

	act, err := st.GetActor(ctx, miner)
	if err != nil {
		return types.Address{}, errors.Wrap(err, "failed to find miner actor in state tree")
	}

	if !act.Code.Equals(types.MinerActorCodeCid) {
		return types.Address{}, fmt.Errorf("address given did not belong to a miner actor")
	}

	var mst core.MinerStorage
	if err := core.UnmarshalStorage(act.ReadStorage(), &mst); err != nil {
		return types.Address{}, err
	}

	return mst.Owner, nil
}

// AddDeal adds a deal by sending a message to the storage market actor on chain
func (stsa *stateTreeMarketPeeker) AddDeal(ctx context.Context, from types.Address, ask, bid uint64, sig string) (*cid.Cid, error) {
	pdata, err := abi.ToEncodedValues(big.NewInt(0).SetUint64(ask), big.NewInt(0).SetUint64(bid), []byte(sig))
	if err != nil {
		return nil, errors.Wrap(err, "failed to encode abi values")
	}

	msg := types.NewMessage(from, core.StorageMarketAddress, nil, "addDeal", pdata)

	err = stsa.nd.AddNewMessage(ctx, msg)
	if err != nil {
		return nil, errors.Wrap(err, "sending 'addDeal' message failed")
	}

	return msg.Cid()
}
