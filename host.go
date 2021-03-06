package redwood

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/brynbellomy/redwood/ctx"
	"github.com/brynbellomy/redwood/types"
)

type Host interface {
	ctx.Logger
	Ctx() *ctx.Context
	Start() error

	// Get(ctx context.Context, url string) (interface{}, error)
	Subscribe(ctx context.Context, stateURI string) (bool, []error)
	SendTx(ctx context.Context, tx Tx) error
	AddRef(reader io.ReadCloser, contentType string) (types.Hash, error)
	AddPeer(ctx context.Context, transportName string, reachableAt StringSet) error
	Transport(name string) Transport
	Controller() Metacontroller
	Address() types.Address
}

type host struct {
	*ctx.Context

	transports        map[string]Transport
	controller        Metacontroller
	signingKeypair    *SigningKeypair
	encryptingKeypair *EncryptingKeypair

	subscriptionsOut map[string]map[peerTuple]*subscriptionOut // map[stateURI][peerTuple]
	peerSeenTxs      map[peerTuple]map[types.ID]bool
	peerSeenTxsMu    sync.RWMutex

	peerStore PeerStore
	refStore  RefStore

	missingRefs   map[types.Hash]struct{}
	chMissingRefs chan []types.Hash
	chFetchRefs   chan struct{}
}

var (
	ErrUnsignedTx = errors.New("unsigned tx")
	ErrProtocol   = errors.New("protocol error")
	ErrPeerIsSelf = errors.New("peer is self")
)

func NewHost(signingKeypair *SigningKeypair, encryptingKeypair *EncryptingKeypair, transports []Transport, controller Metacontroller, refStore RefStore, peerStore PeerStore) (Host, error) {
	transportsMap := make(map[string]Transport)
	for _, tpt := range transports {
		transportsMap[tpt.Name()] = tpt
	}
	h := &host{
		Context:           &ctx.Context{},
		transports:        transportsMap,
		controller:        controller,
		signingKeypair:    signingKeypair,
		encryptingKeypair: encryptingKeypair,
		subscriptionsOut:  make(map[string]map[peerTuple]*subscriptionOut),
		peerSeenTxs:       make(map[peerTuple]map[types.ID]bool),
		peerStore:         peerStore,
		refStore:          refStore,
		missingRefs:       make(map[types.Hash]struct{}),
		chMissingRefs:     make(chan []types.Hash, 100),
		chFetchRefs:       make(chan struct{}),
	}
	return h, nil
}

func (h *host) Ctx() *ctx.Context {
	return h.Context
}

func (h *host) Start() error {
	return h.CtxStart(
		// on startup
		func() error {
			h.SetLogLabel(h.Address().Pretty() + " host")

			// Set up the controller
			h.controller.SetReceivedRefsHandler(h.onReceivedRefs)

			h.CtxAddChild(h.controller.Ctx(), nil)
			err := h.controller.Start()
			if err != nil {
				return err
			}

			// Set up the transports
			for _, transport := range h.transports {
				transport.SetFetchHistoryHandler(h.onFetchHistoryRequestReceived)
				transport.SetTxHandler(h.onTxReceived)
				transport.SetPrivateTxHandler(h.onPrivateTxReceived)
				transport.SetAckHandler(h.onAckReceived)
				transport.SetVerifyAddressHandler(h.onVerifyAddressReceived)
				transport.SetFetchRefHandler(h.onFetchRefReceived)
				h.CtxAddChild(transport.Ctx(), nil)

				err := transport.Start()
				if err != nil {
					return err
				}
			}

			go h.fetchRefsLoop()

			return nil
		},
		nil,
		nil,
		// on shutdown
		func() {},
	)
}

func (h *host) Transport(name string) Transport {
	return h.transports[name]
}

func (h *host) Controller() Metacontroller {
	return h.controller
}

func (h *host) Address() types.Address {
	return h.signingKeypair.Address()
}

func (h *host) onTxReceived(tx Tx, peer Peer) {
	h.Infof(0, "tx %v received", tx.ID.Pretty())
	h.markTxSeenByPeer(peer, tx.ID)

	if !h.controller.HaveTx(tx.URL, tx.ID) {
		err := h.controller.AddTx(&tx)
		if err != nil {
			h.Errorf("error adding tx to controller: %v", err)
		}

		err = h.broadcastTx(context.TODO(), tx)
		if err != nil {
			h.Errorf("error rebroadcasting tx: %v", err)
		}
	}

	err := peer.WriteMsg(Msg{Type: MsgType_Ack, Payload: tx.ID})
	if err != nil {
		h.Errorf("error ACKing peer: %v", err)
	}
}

func (h *host) onPrivateTxReceived(encryptedTx EncryptedTx, peer Peer) {
	h.Infof(0, "private tx %v received", encryptedTx.TxID.Pretty())
	h.markTxSeenByPeer(peer, encryptedTx.TxID)

	bs, err := h.encryptingKeypair.OpenMessageFrom(EncryptingPublicKeyFromBytes(encryptedTx.SenderPublicKey), encryptedTx.EncryptedPayload)
	if err != nil {
		h.Errorf("error decrypting tx: %v", err)
		return
	}

	var tx Tx
	err = json.Unmarshal(bs, &tx)
	if err != nil {
		h.Errorf("error decoding tx: %v", err)
		return
	}

	if encryptedTx.TxID != tx.ID {
		h.Errorf("private tx id does not match")
		return
	}

	if !h.controller.HaveTx(tx.URL, tx.ID) {
		// Add to controller
		err := h.controller.AddTx(&tx)
		if err != nil {
			h.Errorf("error adding tx to controller: %v", err)
		}

		// Broadcast to subscribed peers
		err = h.broadcastTx(context.TODO(), tx)
		if err != nil {
			h.Errorf("error rebroadcasting tx: %v", err)
		}
	}

	err = peer.WriteMsg(Msg{Type: MsgType_Ack, Payload: tx.ID})
	if err != nil {
		h.Errorf("error ACKing peer: %v", err)
	}
}

func (h *host) onAckReceived(txID types.ID, peer Peer) {
	h.Infof(0, "ack received for %v", txID.Hex())
	h.markTxSeenByPeer(peer, txID)
}

func (h *host) markTxSeenByPeer(peer Peer, txID types.ID) {
	h.peerSeenTxsMu.Lock()
	defer h.peerSeenTxsMu.Unlock()

	for _, tuple := range peerTuples(peer) {
		if h.peerSeenTxs[tuple] == nil {
			h.peerSeenTxs[tuple] = make(map[types.ID]bool)
		}
		h.peerSeenTxs[tuple][txID] = true
	}
}

func (h *host) txSeenByPeer(peer Peer, txID types.ID) bool {
	if peer.Address() == (types.Address{}) {
		return false
	}

	h.peerSeenTxsMu.Lock()
	defer h.peerSeenTxsMu.Unlock()

	for _, tuple := range peerTuples(peer) {
		if h.peerSeenTxs[tuple] == nil {
			continue
		}
		if h.peerSeenTxs[tuple][txID] {
			return true
		}
	}
	return false
}

func (h *host) AddPeer(ctx context.Context, transportName string, reachableAt StringSet) error {
	peer, err := h.transports[transportName].GetPeerByConnStrings(ctx, reachableAt)
	if err != nil {
		return err
	}

	err = peer.EnsureConnected(ctx)
	if err != nil {
		return err
	}

	h.peerStore.AddReachableAddresses(transportName, reachableAt)

	sigpubkey, _, err := h.requestPeerCredentials(ctx, peer, h.transports[transportName])
	if err != nil {
		return err
	}

	h.Infof(0, "added peer with address %v", sigpubkey.Address())
	return nil
}

func (h *host) onFetchHistoryRequestReceived(stateURI string, parents []types.ID, toVersion types.ID, peer Peer) error {
	iter := h.controller.FetchTxs(stateURI)
	defer iter.Cancel()

	for {
		tx := iter.Next()
		if iter.Error() != nil {
			return iter.Error()
		} else if tx == nil {
			return nil
		}

		err := peer.WriteMsg(Msg{Type: MsgType_Put, Payload: *tx})
		if err != nil {
			return err
		}
	}
	return nil
}

func (h *host) Subscribe(ctx context.Context, stateURI string) (bool, []error) {
	var anySucceeded bool
	var errs []error
	for _, transport := range h.transports {
		err := h.subscribeWithTransport(ctx, transport, stateURI)
		if err != nil {
			errs = append(errs, err)
		} else {
			anySucceeded = true
		}
	}
	return anySucceeded, errs
}

func (h *host) subscribeWithTransport(ctx context.Context, transport Transport, stateURI string) error {
	ctxFind, cancelFind := context.WithCancel(ctx)
	defer cancelFind()
	ch, err := transport.ForEachProviderOfStateURI(ctxFind, stateURI)
	if err != nil {
		return errors.WithStack(err)
	}

	var peer Peer

	// @@TODO: subscribe to more than one peer?
	for p := range ch {
		err := p.EnsureConnected(ctx)
		if err != nil {
			h.Errorf("error connecting to peer: %v", err)
			continue
		}
		peer = p
		cancelFind()
		break
	}

	if peer == nil {
		return errors.WithStack(ErrNoPeersForURL)
	}

	err = peer.WriteMsg(Msg{Type: MsgType_Subscribe, Payload: stateURI})
	if err != nil {
		return errors.WithStack(err)
	}

	if _, exists := h.subscriptionsOut[stateURI]; !exists {
		h.subscriptionsOut[stateURI] = make(map[peerTuple]*subscriptionOut)
	}
	tuples := peerTuples(peer)
	for _, tuple := range tuples {
		if _, exists := h.subscriptionsOut[stateURI][tuple]; exists {
			return nil
		}
	}

	sub := &subscriptionOut{peer, make(chan struct{})}
	for _, tuple := range tuples {
		h.subscriptionsOut[stateURI][tuple] = sub
	}

	go func() {
		defer peer.CloseConn()
		for {
			select {
			case <-sub.chDone:
				return
			default:
			}

			msg, err := peer.ReadMsg()
			if err != nil {
				h.Errorf("error reading: %v", err)
				return
			}

			if msg.Type != MsgType_Put {
				panic("protocol error")
			}

			tx := msg.Payload.(Tx)
			h.onTxReceived(tx, peer)

			// @@TODO: ACK the PUT
		}
	}()

	return nil
}

func (h *host) requestPeerCredentials(ctx context.Context, peer Peer, transport Transport) (SigningPublicKey, EncryptingPublicKey, error) {
	err := peer.EnsureConnected(ctx)
	if err != nil {
		return nil, nil, err
	}

	challengeMsg, err := types.GenerateChallengeMsg()
	if err != nil {
		return nil, nil, err
	}

	err = peer.WriteMsg(Msg{Type: MsgType_VerifyAddress, Payload: types.ChallengeMsg(challengeMsg)})
	if err != nil {
		return nil, nil, err
	}

	msg, err := peer.ReadMsg()
	if err != nil {
		return nil, nil, err
	} else if msg.Type != MsgType_VerifyAddressResponse {
		return nil, nil, errors.WithStack(ErrProtocol)
	}

	resp, ok := msg.Payload.(VerifyAddressResponse)
	if !ok {
		return nil, nil, errors.WithStack(ErrProtocol)
	}

	sigpubkey, err := RecoverSigningPubkey(types.HashBytes(challengeMsg), resp.Signature)
	if err != nil {
		return nil, nil, err
	}

	encpubkey := EncryptingPublicKeyFromBytes(resp.EncryptingPublicKey)

	peer.SetAddress(sigpubkey.Address())

	h.peerStore.AddVerifiedCredentials(transport.Name(), peer.ReachableAt(), peer.Address(), sigpubkey, encpubkey)

	return sigpubkey, encpubkey, nil
}

func (h *host) onVerifyAddressReceived(challengeMsg types.ChallengeMsg, peer Peer) error {
	defer peer.CloseConn()

	sig, err := h.signingKeypair.SignHash(types.HashBytes(challengeMsg))
	if err != nil {
		return err
	}
	return peer.WriteMsg(Msg{Type: MsgType_VerifyAddressResponse, Payload: VerifyAddressResponse{
		Signature:           sig,
		EncryptingPublicKey: h.encryptingKeypair.EncryptingPublicKey.Bytes(),
	}})
}

type peersWithAddressResult struct {
	Peer
	EncryptingPublicKey
}

func (h *host) peersWithAddress(ctx context.Context, address types.Address) (<-chan peersWithAddressResult, error) {
	if address == h.Address() {
		return nil, errors.WithStack(ErrPeerIsSelf)
	}

	ch := make(chan peersWithAddressResult)
	go func() {
		defer close(ch)

		var alreadySent sync.Map

		if storedPeers := h.peerStore.PeersWithAddress(address); len(storedPeers) > 0 {
			for _, storedPeer := range storedPeers {
				transport, exists := h.transports[storedPeer.transportName]
				if !exists {
					h.Warnf("transport '%v' for no longer exists", storedPeer.transportName)
					continue
				}

				peer, err := transport.GetPeerByConnStrings(ctx, storedPeer.reachableAt)
				if err != nil {
					h.Errorf("error calling transport.GetPeer: %v", err)
					continue
				}
				ch <- peersWithAddressResult{peer, storedPeer.encpubkey}
				for _, tuple := range storedPeer.Tuples() {
					alreadySent.Store(tuple, struct{}{})
				}
			}
		}

		var transportsWg sync.WaitGroup
		for _, transport := range h.transports {

			transportsWg.Add(1)
			transport := transport
			go func() {
				defer transportsWg.Done()

				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				chPeers, err := transport.PeersClaimingAddress(ctx, address)
				if err != nil {
					h.Errorf("error fetching peers with address %v from transport %v", address.Hex(), transport.Name())
					return
				}

				var peersWg sync.WaitGroup
			PeerLoop:
				for peer := range chPeers {
					for _, tuple := range peerTuples(peer) {
						if _, sent := alreadySent.Load(tuple); sent {
							continue PeerLoop
						}
					}

					peersWg.Add(1)
					peer := peer
					go func() {
						defer peersWg.Done()

						err = peer.EnsureConnected(context.TODO())
						if err != nil {
							h.Errorf("error ensuring peer is connected: %v", err)
							return
						}
						defer peer.CloseConn()

						signingPubkey, encryptingPubkey, err := h.requestPeerCredentials(ctx, peer, transport)
						if err != nil {
							h.Errorf("error requesting peer credentials: %v", err)
							return
						} else if signingPubkey.Address() != address {
							h.Errorf("peer sent invalid signature")
							return
						}

						for _, tuple := range peerTuples(peer) {
							alreadySent.Store(tuple, struct{}{})
						}
						ch <- peersWithAddressResult{peer, encryptingPubkey}
					}()
				}
				peersWg.Wait()
			}()
		}

		transportsWg.Wait()
	}()
	return ch, nil
}

func (h *host) broadcastPrivateTxToRecipient(ctx context.Context, txID types.ID, marshalledTx []byte, recipientAddr types.Address) error {
	chPeers, err := h.peersWithAddress(ctx, recipientAddr)
	if err != nil {
		return err
	}

	var anySucceeded bool
	var wg sync.WaitGroup
	for p := range chPeers {
		wg.Add(1)

		p := p
		go func() {
			defer wg.Done()

			err = p.Peer.EnsureConnected(context.TODO())
			if err != nil {
				return
			}
			defer p.Peer.CloseConn()

			msgEncrypted, err := h.encryptingKeypair.SealMessageFor(p.EncryptingPublicKey, marshalledTx)
			if err != nil {
				return
			}

			err = p.Peer.WriteMsg(Msg{
				Type: MsgType_Private,
				Payload: EncryptedTx{
					TxID:             txID,
					EncryptedPayload: msgEncrypted,
					SenderPublicKey:  h.encryptingKeypair.EncryptingPublicKey.Bytes(),
				},
			})
			if err != nil {
				return
			}
			// @@TODO: wait for ack?
			anySucceeded = true
		}()
	}
	wg.Wait()

	if !anySucceeded {
		return errors.Errorf("could not reach recipient %v", recipientAddr.Hex())
	}
	return nil
}

func (h *host) broadcastTx(ctx context.Context, tx Tx) error {
	// @@TODO: should we also send all PUTs to some set of authoritative peers (like a central server)?

	if len(tx.Sig) == 0 {
		return errors.WithStack(ErrUnsignedTx)
	}

	if tx.IsPrivate() {
		marshalledTx, err := json.Marshal(tx)
		if err != nil {
			return errors.WithStack(err)
		}

		var wg sync.WaitGroup
		for _, recipientAddr := range tx.Recipients {
			if recipientAddr == h.Address() {
				continue
			}

			wg.Add(1)
			go func() {
				defer wg.Done()

				err := h.broadcastPrivateTxToRecipient(ctx, tx.ID, marshalledTx, recipientAddr)
				if err != nil {
					h.Errorf(err.Error())
				}
			}()
		}
		wg.Wait()

	} else {
		// @@TODO: do we need to trim the tx's patches' keypaths so that they don't include
		// the keypath that the subscription is listening to?

		var wg sync.WaitGroup
		for _, transport := range h.transports {
			wg.Add(1)

			transport := transport
			go func() {
				defer wg.Done()

				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				ch, err := transport.ForEachSubscriberToStateURI(ctx, tx.URL)
				if err != nil {
					h.Errorf("error fetching subscribers to url '%v' from transport %v", tx.URL, transport.Name())
					return
				}

				var peerWg sync.WaitGroup
				for peer := range ch {
					if h.txSeenByPeer(peer, tx.ID) {
						h.Errorf("tx already seen by peer %v %v", peer.Transport().Name(), peer.Address())
						continue
					}
					h.Errorf("tx NOT already seen by peer %v %v", peer.Transport().Name(), peer.Address())

					peerWg.Add(1)
					peer := peer
					go func() {
						defer peerWg.Done()

						err := peer.EnsureConnected(context.TODO())
						if err != nil {
							h.Errorf("error connecting to peer: %v", err)
							return
						}

						err = peer.WriteMsg(Msg{Type: MsgType_Put, Payload: tx})
						if err != nil {
							h.Errorf("error writing tx to peer: %v", err)
							return
						}
					}()
				}
				peerWg.Wait()
			}()
		}
		wg.Wait()
	}
	return nil
}

func (h *host) SendTx(ctx context.Context, tx Tx) error {
	h.Info(0, "adding tx ", tx.ID.Pretty())

	if len(tx.Sig) == 0 {
		err := h.SignTx(&tx)
		if err != nil {
			return err
		}
	}

	err := h.controller.AddTx(&tx)
	if err != nil {
		return err
	}

	err = h.broadcastTx(h.Ctx(), tx)
	if err != nil {
		return err
	}

	return nil
}

func (h *host) SignTx(tx *Tx) error {
	var err error
	tx.Sig, err = h.signingKeypair.SignHash(tx.Hash())
	return err
}

func (h *host) AddRef(reader io.ReadCloser, contentType string) (types.Hash, error) {
	return h.refStore.StoreObject(reader, contentType)
}

func (h *host) fetchRefsLoop() {
	tick := time.NewTicker(10 * time.Second) // @@TODO: make configurable
	defer tick.Stop()

	for {
		select {
		case <-h.Ctx().Done():
			return

		case refs := <-h.chMissingRefs:
			for _, ref := range refs {
				h.missingRefs[ref] = struct{}{}
			}

			h.fetchMissingRefs()

		case <-tick.C:
			if len(h.missingRefs) > 0 {
				h.fetchMissingRefs()
			}
		}
	}
}

func (h *host) onReceivedRefs(refs []types.Hash) {
	if len(refs) == 0 {
		return
	}

	select {
	case <-h.Ctx().Done():
		return
	case h.chMissingRefs <- refs:
	}
}

func (h *host) fetchMissingRefs() {
	var fetchedAny bool
	defer func() {
		if fetchedAny {
			h.controller.OnDownloadedRef()
		}
	}()

	var succeeded sync.Map
	var wg sync.WaitGroup
	for ref := range h.missingRefs {
		if h.refStore.HaveObject(ref) {
			succeeded.Store(ref, struct{}{})
			continue
		}

		wg.Add(1)
		ref := ref
		go func() {
			defer wg.Done()
			success := h.fetchRef(ref)
			if success {
				fetchedAny = true
				succeeded.Store(ref, struct{}{})
			}
		}()
	}
	wg.Wait()

	succeeded.Range(func(key interface{}, _ interface{}) bool {
		delete(h.missingRefs, key.(types.Hash))
		return true
	})
}

func (h *host) fetchRef(ref types.Hash) bool {
	chPeers := make(chan Peer)
	ctx, cancel := context.WithCancel(h.Ctx())
	defer cancel()

	for _, transport := range h.transports {
		transport := transport
		go func() {
			ch, err := transport.ForEachProviderOfRef(ctx, ref)
			if err != nil {
				h.Errorf("error finding providers of ref %v from transport %v: %v", ref.String(), transport.Name(), err)
				return
			}
			for peer := range ch {
				select {
				case chPeers <- peer:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	for peer := range chPeers {
		err := peer.EnsureConnected(ctx)
		if err != nil {
			h.Errorf("error connecting to peer: %v", err)
			continue
		}

		err = peer.WriteMsg(Msg{Type: MsgType_FetchRef, Payload: ref})
		if err != nil {
			h.Errorf("error writing to peer: %v", err)
			continue
		}

		var msg Msg
		msg, err = peer.ReadMsg()
		if err != nil {
			h.Errorf("error reading from peer: %v", err)
			continue
		} else if msg.Type != MsgType_FetchRefResponse {
			h.Errorf("protocol probs")
			continue
		}

		resp, is := msg.Payload.(FetchRefResponse)
		if !is {
			h.Errorf("protocol probs")
			continue
		} else if resp.Header == nil {
			h.Errorf("protocol probs")
			continue
		}

		pr, pw := io.Pipe()
		go func() {
			var err error
			defer func() { pw.CloseWithError(err) }()

			for {
				select {
				case <-ctx.Done():
					err = ctx.Err()
					return
				default:
				}

				var msg Msg
				msg, err = peer.ReadMsg()
				if err != nil {
					return
				} else if msg.Type != MsgType_FetchRefResponse {
					err = errors.New("protocol probs")
					return
				}

				resp, is := msg.Payload.(FetchRefResponse)
				if !is {
					err = errors.New("protocol probs")
					return
				} else if resp.Body == nil {
					err = errors.New("protocol probs")
					return
				} else if resp.Body.End {
					return
				}

				var n int
				n, err = pw.Write(resp.Body.Data)
				if err != nil {
					return
				} else if n < len(resp.Body.Data) {
					err = io.ErrUnexpectedEOF
					return
				}
			}
		}()

		hash, err := h.refStore.StoreObject(pr, "application/octet-stream")
		if err != nil {
			h.Errorf("protocol probs: %v", err)
			continue
		}
		h.Infof(0, "stored ref %v", hash)
		// @@TODO: check stored refHash against the one we requested

		for _, transport := range h.transports {
			err = transport.AnnounceRef(hash)
			if err != nil {
				h.Errorf("error announcing ref %v over transport %v: %v", hash.String(), transport.Name(), err)
				// this is a non-critical error, don't bail out
			}
		}
		return true
	}
	return false
}

const (
	REF_CHUNK_SIZE = 1024 // @@TODO: tunable buffer size?
)

func (h *host) onFetchRefReceived(refHash types.Hash, peer Peer) {
	defer peer.CloseConn()

	objectReader, _, err := h.refStore.Object(refHash)
	// @@TODO: handle the case where we don't have the ref more gracefully
	if err != nil {
		panic(err)
	}

	err = peer.WriteMsg(Msg{Type: MsgType_FetchRefResponse, Payload: FetchRefResponse{Header: &FetchRefResponseHeader{}}})
	if err != nil {
		h.Errorf("[ref server] %+v", errors.WithStack(err))
		return
	}

	buf := make([]byte, REF_CHUNK_SIZE)
	for {
		n, err := io.ReadFull(objectReader, buf)
		if err == io.EOF {
			break
		} else if err == io.ErrUnexpectedEOF {
			buf = buf[:n]
		} else if err != nil {
			h.Errorf("[ref server] %+v", err)
			return
		}

		err = peer.WriteMsg(Msg{Type: MsgType_FetchRefResponse, Payload: FetchRefResponse{Body: &FetchRefResponseBody{Data: buf}}})
		if err != nil {
			h.Errorf("[ref server] %+v", errors.WithStack(err))
			return
		}
	}

	err = peer.WriteMsg(Msg{Type: MsgType_FetchRefResponse, Payload: FetchRefResponse{Body: &FetchRefResponseBody{End: true}}})
	if err != nil {
		h.Errorf("[ref server] %+v", errors.WithStack(err))
		return
	}
}
