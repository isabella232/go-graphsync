package peerresponsemanager

import (
	"context"
	"sync"

	"github.com/ipfs/go-graphsync"
	"github.com/ipfs/go-graphsync/peermanager"

	cidlink "github.com/ipld/go-ipld-prime/linking/cid"

	logging "github.com/ipfs/go-log"
	"github.com/ipld/go-ipld-prime"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-graphsync/linktracker"
	gsmsg "github.com/ipfs/go-graphsync/message"
	"github.com/ipfs/go-graphsync/responsemanager/responsebuilder"
	"github.com/libp2p/go-libp2p-core/peer"
)

const (
	// max block size is the maximum size for batching blocks in a single payload
	maxBlockSize uint64 = 512 * 1024
)

var log = logging.Logger("graphsync")

// PeerMessageHandler is an interface that can send a response for a given peer across
// the network.
type PeerMessageHandler interface {
	SendResponse(peer.ID, []gsmsg.GraphSyncResponse, []blocks.Block) <-chan struct{}
}

type peerResponseSender struct {
	p            peer.ID
	ctx          context.Context
	cancel       context.CancelFunc
	peerHandler  PeerMessageHandler
	outgoingWork chan struct{}

	linkTrackerLk      sync.RWMutex
	linkTracker        *linktracker.LinkTracker
	responseBuildersLk sync.RWMutex
	responseBuilders   []*responsebuilder.ResponseBuilder
}

// PeerResponseSender handles batching, deduping, and sending responses for
// a given peer across multiple requests.
type PeerResponseSender interface {
	peermanager.PeerProcess
	SendResponse(
		requestID graphsync.RequestID,
		link ipld.Link,
		data []byte,
	) graphsync.BlockData
	SendExtensionData(graphsync.RequestID, graphsync.ExtensionData)
	FinishRequest(requestID graphsync.RequestID) graphsync.ResponseStatusCode
	FinishWithError(requestID graphsync.RequestID, status graphsync.ResponseStatusCode)
	PauseRequest(requestID graphsync.RequestID)
}

// NewResponseSender generates a new PeerResponseSender for the given context, peer ID,
// using the given peer message handler.
func NewResponseSender(ctx context.Context, p peer.ID, peerHandler PeerMessageHandler) PeerResponseSender {
	ctx, cancel := context.WithCancel(ctx)
	return &peerResponseSender{
		p:            p,
		ctx:          ctx,
		cancel:       cancel,
		peerHandler:  peerHandler,
		outgoingWork: make(chan struct{}, 1),
		linkTracker:  linktracker.New(),
	}
}

// Startup initiates message sending for a peer
func (prm *peerResponseSender) Startup() {
	go prm.run()
}

// Shutdown stops sending messages for a peer
func (prm *peerResponseSender) Shutdown() {
	prm.cancel()
}

func (prm *peerResponseSender) SendExtensionData(requestID graphsync.RequestID, extension graphsync.ExtensionData) {
	if prm.buildResponse(0, func(responseBuilder *responsebuilder.ResponseBuilder) {
		responseBuilder.AddExtensionData(requestID, extension)
	}) {
		prm.signalWork()
	}
}

type blockData struct {
	link      ipld.Link
	blockSize uint64
	sendBlock bool
}

func (bd blockData) Link() ipld.Link {
	return bd.link
}

func (bd blockData) BlockSize() uint64 {
	return bd.blockSize
}

func (bd blockData) BlockSizeOnWire() uint64 {
	if !bd.sendBlock {
		return 0
	}
	return bd.blockSize
}

// SendResponse sends a given link for a given
// requestID across the wire, as well as its corresponding
// block if the block is present and has not already been sent
// it returns the number of block bytes sent
func (prm *peerResponseSender) SendResponse(
	requestID graphsync.RequestID,
	link ipld.Link,
	data []byte,
) graphsync.BlockData {
	hasBlock := data != nil
	prm.linkTrackerLk.Lock()
	sendBlock := hasBlock && prm.linkTracker.BlockRefCount(link) == 0
	blkSize := uint64(len(data))
	bd := blockData{link, blkSize, sendBlock}
	prm.linkTracker.RecordLinkTraversal(requestID, link, hasBlock)
	prm.linkTrackerLk.Unlock()

	if prm.buildResponse(bd.BlockSizeOnWire(), func(responseBuilder *responsebuilder.ResponseBuilder) {
		if sendBlock {
			cidLink := link.(cidlink.Link)
			block, err := blocks.NewBlockWithCid(data, cidLink.Cid)
			if err != nil {
				log.Errorf("Data did not match cid when sending link for %s", cidLink.String())
			}
			responseBuilder.AddBlock(block)
		}
		responseBuilder.AddLink(requestID, link, hasBlock)
	}) {
		prm.signalWork()
	}
	return bd
}

// FinishRequest marks the given requestID as having sent all responses
func (prm *peerResponseSender) FinishRequest(requestID graphsync.RequestID) graphsync.ResponseStatusCode {
	prm.linkTrackerLk.Lock()
	isComplete := prm.linkTracker.FinishRequest(requestID)
	prm.linkTrackerLk.Unlock()
	var status graphsync.ResponseStatusCode
	if isComplete {
		status = graphsync.RequestCompletedFull
	} else {
		status = graphsync.RequestCompletedPartial
	}
	prm.finish(requestID, status)
	return status
}

// FinishWithError marks the given requestID as having terminated with an error
func (prm *peerResponseSender) FinishWithError(requestID graphsync.RequestID, status graphsync.ResponseStatusCode) {
	prm.linkTrackerLk.Lock()
	prm.linkTracker.FinishRequest(requestID)
	prm.linkTrackerLk.Unlock()

	prm.finish(requestID, status)
}

func (prm *peerResponseSender) PauseRequest(requestID graphsync.RequestID) {
	prm.finish(requestID, graphsync.RequestPaused)
}

func (prm *peerResponseSender) finish(requestID graphsync.RequestID, status graphsync.ResponseStatusCode) {
	if prm.buildResponse(0, func(responseBuilder *responsebuilder.ResponseBuilder) {
		responseBuilder.AddResponseCode(requestID, status)
	}) {
		prm.signalWork()
	}
}
func (prm *peerResponseSender) buildResponse(blkSize uint64, buildResponseFn func(*responsebuilder.ResponseBuilder)) bool {
	prm.responseBuildersLk.Lock()
	defer prm.responseBuildersLk.Unlock()
	if shouldBeginNewResponse(prm.responseBuilders, blkSize) {
		prm.responseBuilders = append(prm.responseBuilders, responsebuilder.New())
	}
	responseBuilder := prm.responseBuilders[len(prm.responseBuilders)-1]
	buildResponseFn(responseBuilder)
	return !responseBuilder.Empty()
}

func shouldBeginNewResponse(responseBuilders []*responsebuilder.ResponseBuilder, blkSize uint64) bool {
	if len(responseBuilders) == 0 {
		return true
	}
	if blkSize == 0 {
		return false
	}
	return responseBuilders[len(responseBuilders)-1].BlockSize()+blkSize > maxBlockSize
}

func (prm *peerResponseSender) signalWork() {
	select {
	case prm.outgoingWork <- struct{}{}:
	default:
	}
}

func (prm *peerResponseSender) run() {
	for {
		select {
		case <-prm.ctx.Done():
			return
		case <-prm.outgoingWork:
			prm.sendResponseMessages()
		}
	}
}

func (prm *peerResponseSender) sendResponseMessages() {
	prm.responseBuildersLk.Lock()
	builders := prm.responseBuilders
	prm.responseBuilders = nil
	prm.responseBuildersLk.Unlock()

	for _, builder := range builders {
		if builder.Empty() {
			continue
		}
		responses, blks, err := builder.Build()
		if err != nil {
			log.Errorf("Unable to assemble GraphSync response: %s", err.Error())
		}

		done := prm.peerHandler.SendResponse(prm.p, responses, blks)

		// wait for message to be processed
		select {
		case <-done:
		case <-prm.ctx.Done():
		}
	}

}
