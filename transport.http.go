package redwood

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/brynbellomy/redwood/ctx"
)

type httpTransport struct {
	*ctx.Context

	address Address
	store   Store
	ownURL  string
	port    uint

	ackHandler           AckHandler
	putHandler           PutHandler
	verifyAddressHandler VerifyAddressHandler

	subscriptionsIn   map[string][]*httpSubscriptionIn
	subscriptionsInMu sync.RWMutex
}

func NewHTTPTransport(addr Address, port uint, store Store) (Transport, error) {
	t := &httpTransport{
		Context:         &ctx.Context{},
		address:         addr,
		subscriptionsIn: make(map[string][]*httpSubscriptionIn),
		store:           store,
		port:            port,
		ownURL:          fmt.Sprintf("localhost:%v", port),
	}
	return t, nil
}

func (t *httpTransport) Start() error {
	return t.CtxStart(
		// on startup
		func() error {
			t.Infof(0, "opening http transport at :%v", t.port)
			t.SetLogLabel(t.address.Pretty() + " transport")
			go func() {
				err := http.ListenAndServe(fmt.Sprintf(":%v", t.port), t)
				if err != nil {
					panic(err.Error())
				}
			}()
			return nil
		},
		nil,
		nil,
		// on shutdown
		nil,
	)
}

type httpSubscriptionIn struct {
	io.Writer
	http.Flusher
	chDone chan struct{}
}

func (s *httpSubscriptionIn) Close() error {
	close(s.chDone)
	return nil
}

func (t *httpTransport) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	switch r.Method {
	case "GET":
		if challengeMsgHex := r.Header.Get("Verify-Credentials"); challengeMsgHex != "" {
			//
			// Address verification request
			//
			t.Infof(0, "incoming verify-address request")

			challengeMsg, err := hex.DecodeString(challengeMsgHex)
			if err != nil {
				http.Error(w, "Verify-Credentials header: bad challenge message", http.StatusBadRequest)
				return
			}

			verifyAddressResponse, err := t.verifyAddressHandler([]byte(challengeMsg))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			err = json.NewEncoder(w).Encode(verifyAddressResponse)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

		} else if r.Header.Get("Subscribe") != "" {
			//
			// Subscription request
			//
			t.Infof(0, "incoming subscription")

			// Make sure that the writer supports flushing.
			f, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "streaming unsupported", http.StatusInternalServerError)
				return
			}

			// Listen to the closing of the http connection via the CloseNotifier
			notify := w.(http.CloseNotifier).CloseNotify()
			go func() {
				<-notify
				t.Info(0, "http connection closed")
			}()

			// Set the headers related to event streaming.
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			// w.Header().Set("Transfer-Encoding", "chunked")

			sub := &httpSubscriptionIn{w, f, make(chan struct{})}

			urlToSubscribe := r.Header.Get("Subscribe")

			t.subscriptionsInMu.Lock()
			t.subscriptionsIn[urlToSubscribe] = append(t.subscriptionsIn[urlToSubscribe], sub)
			t.subscriptionsInMu.Unlock()

			f.Flush()

			// Block until the subscription is canceled
			<-sub.chDone

		} else {
			//
			// Regular HTTP GET request (from browsers, etc.)
			//

			// @@TODO: this is hacky
			if r.URL.Path == "/braid.js" {
				var filename string
				if fileExists("./braidjs/dist.js") {
					filename = "./braidjs/dist.js"
				} else if fileExists("../braidjs/dist.js") {
					filename = "../braidjs/dist.js"
				}
				f, err := os.Open(filename)
				if err != nil {
					http.Error(w, "can't find braidjs", http.StatusNotFound)
					return
				}
				defer f.Close()
				http.ServeContent(w, r, "./braidjs/dist.js", time.Now(), f)
				return
			}

			keypath := filterEmptyStrings(strings.Split(r.URL.Path[1:], "/"))
			stateMap, isMap := t.store.State().(map[string]interface{})
			if !isMap {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}

			val, exists := M(stateMap).GetValue(keypath...)
			if !exists {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}

			if r.Header.Get("Accept") == "application/json" {
				var resp struct {
					MostRecentTxHash Hash        `json:"mostRecentTxHash"`
					Data             interface{} `json:"data"`
				}
				resp.MostRecentTxHash = t.store.MostRecentTxHash() // @@TODO: hacky

				switch v := val.(type) {
				case string:
					resp.Data = v

				case []byte:
					resp.Data = string(v) // @@TODO: probably don't want this

				case map[string]interface{}, []interface{}:
					resp.Data = v

				default:
					http.Error(w, "not found", http.StatusNotFound)
				}

				j, err := json.Marshal(resp)
				if err != nil {
					panic(err)
				}

				_, err = io.Copy(w, bytes.NewBuffer(j))
				if err != nil {
					panic(err)
				}

			} else {
				switch v := val.(type) {
				case string:
					_, err := io.Copy(w, bytes.NewBuffer([]byte(v)))
					if err != nil {
						panic(err)
					}

				case []byte:
					_, err := io.Copy(w, bytes.NewBuffer(v))
					if err != nil {
						panic(err)
					}

				case map[string]interface{}, []interface{}:
					j, err := json.Marshal(v)
					if err != nil {
						panic(err)
					}
					_, err = io.Copy(w, bytes.NewBuffer(j))
					if err != nil {
						panic(err)
					}

				default:
					http.Error(w, "not found", http.StatusNotFound)
				}
			}
		}

	case "ACK":
		defer r.Body.Close()

		bs, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Errorf("error reading ACK body: %v", err)
			http.Error(w, "error reading body", http.StatusBadRequest)
			return
		}

		var txHash Hash
		err = txHash.UnmarshalText(bs)
		if err != nil {
			t.Errorf("error reading ACK body: %v", err)
			http.Error(w, "error reading body", http.StatusBadRequest)
			return
		}

		t.ackHandler(txHash, &httpPeer{t, r.RemoteAddr, w, nil, nil, httpPeerState_Unknown, nil, nil})

	case "PUT":
		defer r.Body.Close()

		t.Infof(0, "incoming tx")

		var tx Tx
		err := json.NewDecoder(r.Body).Decode(&tx)
		if err != nil {
			panic(err)
		}

		t.putHandler(tx, &httpPeer{t, r.RemoteAddr, w, nil, nil, httpPeerState_Unknown, nil, nil})

	default:
		http.Error(w, "unsupported method", http.StatusMethodNotAllowed)
	}
}

func (t *httpTransport) SetPutHandler(handler PutHandler) {
	t.putHandler = handler
}

func (t *httpTransport) SetAckHandler(handler AckHandler) {
	t.ackHandler = handler
}

func (t *httpTransport) SetVerifyAddressHandler(handler VerifyAddressHandler) {
	t.verifyAddressHandler = handler
}

func (t *httpTransport) AddPeer(ctx context.Context, addrString string) (Peer, error) {
	return &httpPeer{t: t, url: "http://" + addrString}, nil
}

func (t *httpTransport) ForEachProviderOfURL(ctx context.Context, theURL string, fn func(Peer) (bool, error)) error {
	// theURL = braidURLToHTTP(theURL)

	u, err := url.Parse("http://" + theURL)
	if err != nil {
		return err
	}

	u.Path = path.Join(u.Path, "providers")

	resp, err := http.Get(u.String())
	if err != nil {
		return err
	} else if resp.StatusCode != 200 {
		return errors.Errorf("error GETting providers: (%v) %v", resp.StatusCode, resp.Status)
	}
	defer resp.Body.Close()

	var providers []string
	err = json.NewDecoder(resp.Body).Decode(&providers)
	if err != nil {
		return err
	}

	for _, providerURL := range providers {
		if providerURL == t.ownURL {
			continue
		}

		keepGoing, err := fn(&httpPeer{t, providerURL, nil, nil, nil, httpPeerState_Unknown, nil, nil})
		if err != nil {
			return errors.WithStack(err)
		} else if !keepGoing {
			break
		}
	}
	return nil
}

func (t *httpTransport) ForEachSubscriberToURL(ctx context.Context, theURL string, fn func(Peer) (bool, error)) error {
	// theURL = braidURLToHTTP(theURL)

	u, err := url.Parse("http://" + theURL)
	if err != nil {
		return errors.WithStack(err)
	}

	domain := u.Host

	t.subscriptionsInMu.RLock()
	defer t.subscriptionsInMu.RUnlock()

	for _, sub := range t.subscriptionsIn[domain] {
		keepGoing, err := fn(&httpPeer{t, "", sub.Writer, nil, sub.Flusher, httpPeerState_Unknown, nil, nil})
		if err != nil {
			return errors.WithStack(err)
		} else if !keepGoing {
			break
		}
	}
	return nil
}

func (t *httpTransport) PeersWithAddress(ctx context.Context, address Address) (<-chan Peer, error) {
	panic("unimplemented")
}

type httpPeer struct {
	t *httpTransport

	// stream
	url string
	io.Writer
	io.ReadCloser
	http.Flusher

	// state
	peerState     httpPeerState
	challengeResp []byte

	// identity
	encryptingPublicKey EncryptingPublicKey
}

type httpPeerState int

const (
	httpPeerState_Unknown httpPeerState = iota
	httpPeerState_VerifyingAddress
)

func (p *httpPeer) ID() string {
	return p.url
}

func (p *httpPeer) EnsureConnected(ctx context.Context) error {
	return nil
}

func (p *httpPeer) WriteMsg(msg Msg) error {
	switch msg.Type {
	case MsgType_Subscribe:
		urlToSubscribe, ok := msg.Payload.(string)
		if !ok {
			return ErrProtocol
		}

		// url = braidURLToHTTP(url)

		client := http.Client{}
		req, err := http.NewRequest("GET", "http://"+p.url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Cache-Control", "no-cache")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Connection", "keep-alive")
		req.Header.Set("Subscribe", urlToSubscribe)

		resp, err := client.Do(req)
		if err != nil {
			return err
		} else if resp.StatusCode != 200 {
			return errors.Errorf("error GETting peer: (%v) %v", resp.StatusCode, resp.Status)
		}

		p.ReadCloser = resp.Body

	case MsgType_Put:
		if p.Writer != nil {
			// This peer is subscribed, so we have a connection open already
			err := WriteMsg(p, msg)
			if err != nil {
				return err
			}
			if p.Flusher != nil {
				p.Flusher.Flush()
			}

		} else {
			// This peer is not subscribed, so we make a PUT
			bs, err := json.Marshal(msg)
			if err != nil {
				return err
			}

			client := http.Client{}
			req, err := http.NewRequest("PUT", p.url, bytes.NewReader(bs))
			if err != nil {
				return err
			}

			resp, err := client.Do(req)
			if err != nil {
				return err
			} else if resp.StatusCode != 200 {
				return errors.Errorf("error PUTting to peer: (%v) %v", resp.StatusCode, resp.Status)
			}
			defer resp.Body.Close()
		}

	case MsgType_Ack:
		txHash, ok := msg.Payload.(Hash)
		if !ok {
			return ErrProtocol
		}

		vidBytes, err := txHash.MarshalText()
		if err != nil {
			return errors.WithStack(err)
		}

		client := http.Client{}
		req, err := http.NewRequest("ACK", "http://"+p.url, bytes.NewReader(vidBytes))
		if err != nil {
			return err
		}

		resp, err := client.Do(req)
		if err != nil {
			return err
		} else if resp.StatusCode != 200 {
			return errors.Errorf("error ACKing to peer: (%v) %v", resp.StatusCode, resp.Status)
		}
		defer resp.Body.Close()

	case MsgType_VerifyAddress:
		challengeMsg, ok := msg.Payload.([]byte)
		if !ok {
			return ErrProtocol
		}

		client := http.Client{}
		req, err := http.NewRequest("GET", p.url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Verify-Credentials", hex.EncodeToString(challengeMsg))

		resp, err := client.Do(req)
		if err != nil {
			return err
		} else if resp.StatusCode != 200 {
			return errors.Errorf("error verifying peer address: (%v) %v", resp.StatusCode, resp.Status)
		}

		p.ReadCloser = resp.Body
		p.peerState = httpPeerState_VerifyingAddress

	default:
		panic("unimplemented")
	}
	return nil
}

func (p *httpPeer) ReadMsg() (Msg, error) {
	switch p.peerState {
	case httpPeerState_VerifyingAddress:
		p.peerState = httpPeerState_Unknown

		var verifyResp VerifyAddressResponse
		err := json.NewDecoder(p.ReadCloser).Decode(&verifyResp)
		if err != nil {
			return Msg{}, err
		}

		p.challengeResp = verifyResp.Signature
		p.encryptingPublicKey = EncryptingPublicKeyFromBytes(verifyResp.EncryptingPublicKey)

		return Msg{Type: MsgType_VerifyAddressResponse, Payload: verifyResp}, nil

	default:
		var msg Msg
		err := ReadMsg(p.ReadCloser, &msg)
		return msg, err
	}
}

func (p *httpPeer) CloseConn() error {
	if p.ReadCloser != nil {
		return p.ReadCloser.Close()
	}
	return nil
}
