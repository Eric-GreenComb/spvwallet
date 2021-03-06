package spvwallet

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/peer"
	"github.com/btcsuite/btcd/wire"
	"time"
)

var (
	maxHash              *chainhash.Hash
	MAX_UNCONFIRMED_TIME time.Duration = time.Hour * 24 * 7
)

func init() {
	h, err := chainhash.NewHashFromStr("0000000000000000000000000000000000000000000000000000000000000000")
	if err != nil {
		log.Fatal(err)
	}
	maxHash = h
}

func (w *SPVWallet) startChainDownload(p *peer.Peer) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("Unhandled error in startChainDownload", r)
		}
	}()
	w.mutex.Lock()
	defer w.mutex.Unlock()
	if w.blockchain.ChainState() == SYNCING {
		height, _ := w.blockchain.db.Height()
		if height >= uint32(p.LastBlock()) {
			moar := w.peerManager.CheckForMoreBlocks(height)
			if !moar {
				log.Info("Chain download complete")
				w.blockchain.SetChainState(WAITING)
				w.Rebroadcast()
				close(w.blockQueue)
			}
			return
		}
		gBlocks := wire.NewMsgGetBlocks(maxHash)
		hashes := w.blockchain.GetBlockLocatorHashes()
		gBlocks.BlockLocatorHashes = hashes
		p.QueueMessage(gBlocks, nil)
	}
}

func (w *SPVWallet) onMerkleBlock(p *peer.Peer, m *wire.MsgMerkleBlock) {
	if w.blockchain.ChainState() == SYNCING && w.peerManager.DownloadPeer() != nil && w.peerManager.DownloadPeer().ID() == p.ID() {
		queueHash := <-w.blockQueue
		headerHash := m.Header.BlockHash()
		if !headerHash.IsEqual(&queueHash) {
			log.Errorf("Peer%d is sending us blocks out of order", p.ID())
			p.Disconnect()
			return
		}
	}
	txids, err := checkMBlock(m)
	if err != nil {
		log.Errorf("Peer%d sent an invalid MerkleBlock", p.ID())
		p.Disconnect()
		return
	}
	newBlock, reorg, height, err := w.blockchain.CommitHeader(m.Header)
	if err != nil {
		log.Warning(err)
		return
	}
	if !newBlock {
		return
	}

	// We hit a reorg. Rollback the transactions and resync from the reorg point.
	if reorg != nil {
		err := w.txstore.processReorg(reorg.height)
		if err != nil {
			log.Error(err)
		}
		w.blockchain.SetChainState(SYNCING)
		w.blockchain.db.Put(*reorg, true)
		go w.startChainDownload(p)
		return
	}

	for _, txid := range txids {
		w.mutex.Lock()
		w.toDownload[*txid] = int32(height)
		w.mutex.Unlock()
	}
	log.Debugf("Received Merkle Block %s at height %d\n", m.Header.BlockHash().String(), height)
	if len(w.blockQueue) == 0 && w.blockchain.ChainState() == SYNCING {
		go w.startChainDownload(p)
	}
	if w.blockchain.ChainState() == WAITING {
		txns, err := w.txstore.Txns().GetAll(false)
		if err != nil {
			log.Error(err)
			return
		}
		now := time.Now()
		for i := len(txns) - 1; i >= 0; i-- {
			if now.After(txns[i].Timestamp.Add(MAX_UNCONFIRMED_TIME)) && txns[i].Height == int32(0) {
				log.Noticef("Marking tx as dead %s", txns[i].Txid)
				h, err := chainhash.NewHashFromStr(txns[i].Txid)
				if err != nil {
					log.Error(err)
					continue
				}
				err = w.txstore.markAsDead(*h)
				if err != nil {
					log.Error(err)
					continue
				}
			}
		}
	}
}

func (w *SPVWallet) onTx(p *peer.Peer, m *wire.MsgTx) {
	w.mutex.RLock()
	height := w.toDownload[m.TxHash()]
	w.mutex.RUnlock()
	hits, err := w.txstore.Ingest(m, height)
	if err != nil {
		log.Errorf("Error ingesting tx: %s\n", err.Error())
		return
	}
	if hits == 0 {
		log.Debugf("Tx %s from Peer%d had no hits, filter false positive.", m.TxHash().String(), p.ID())
		w.fPositives <- p
		return
	}
	w.updateFilterAndSend(p)
	log.Infof("Tx %s from Peer%d ingested and matches %d utxo/adrs.", m.TxHash().String(), p.ID(), hits)

	// FIXME: right now the hash stays in memory forever. We need to delete it but the way the code works,
	// FIXME: doing so will cause the height to get reset to zero if a peer relays the tx to us again.
}

func (w *SPVWallet) onInv(p *peer.Peer, m *wire.MsgInv) {
	go func() {
		defer func() {
			if err := recover(); err != nil {
				log.Error(err)
			}
		}()
		for _, inv := range m.InvList {
			switch inv.Type {
			case wire.InvTypeBlock:
				// Kind of lame to send separate getData messages but this allows us
				// to take advantage of the timeout on the upper layer. Otherwise we
				// need separate timeout handling.
				inv.Type = wire.InvTypeFilteredBlock
				gData := wire.NewMsgGetData()
				gData.AddInvVect(inv)
				p.QueueMessage(gData, nil)
				if w.blockchain.ChainState() == SYNCING && w.peerManager.DownloadPeer() != nil && w.peerManager.DownloadPeer().ID() == p.ID() {
					w.blockQueue <- inv.Hash
				}
			case wire.InvTypeTx:
				gData := wire.NewMsgGetData()
				gData.AddInvVect(inv)
				p.QueueMessage(gData, nil)
			default:
				continue
			}

		}
	}()
}

func (w *SPVWallet) onGetData(p *peer.Peer, m *wire.MsgGetData) {
	log.Debugf("Received getdata request from Peer%d\n", p.ID())
	var sent int32
	for _, thing := range m.InvList {
		if thing.Type == wire.InvTypeTx {
			tx, _, err := w.txstore.Txns().Get(thing.Hash)
			if err != nil {
				log.Errorf("Error getting tx %s: %s", thing.Hash.String(), err.Error())
				continue
			}
			p.QueueMessage(tx, nil)
			sent++
			continue
		}
		// didn't match, so it's not something we're responding to
		log.Debugf("We only respond to tx requests, ignoring")

	}
	log.Debugf("Sent %d of %d requested items to Peer%d", sent, len(m.InvList), p.ID())
}

func (w *SPVWallet) fPositiveHandler(quit chan int) {
exit:
	for {
		select {
		case peer := <-w.fPositives:
			w.mutex.RLock()
			falsePostives, _ := w.fpAccumulator[peer.ID()]
			w.mutex.RUnlock()
			falsePostives++
			if falsePostives > 7 {
				w.updateFilterAndSend(peer)
				log.Debugf("Reset %d false positives for Peer%d\n", falsePostives, peer.ID())
				// reset accumulator
				falsePostives = 0
			}
			w.mutex.Lock()
			w.fpAccumulator[peer.ID()] = falsePostives
			w.mutex.Unlock()
		case <-quit:
			break exit
		}
	}
}

func (w *SPVWallet) updateFilterAndSend(p *peer.Peer) {
	filt, err := w.txstore.GimmeFilter()
	if err != nil {
		log.Errorf("Error creating filter: %s\n", err.Error())
		return
	}
	// send filter
	p.QueueMessage(filt.MsgFilterLoad(), nil)
	log.Debugf("Sent filter to Peer%d\n", p.ID())
}

func (w *SPVWallet) Rebroadcast() {
	// get all unconfirmed txs
	invMsg, err := w.txstore.GetPendingInv()
	if err != nil {
		log.Errorf("Rebroadcast error: %s", err.Error())
	}
	if len(invMsg.InvList) == 0 { // nothing to broadcast, so don't
		return
	}
	for _, peer := range w.peerManager.ConnectedPeers() {
		peer.QueueMessage(invMsg, nil)
	}
}
