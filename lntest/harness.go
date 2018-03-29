package lntest

import (
	"fmt"
	"io/ioutil"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/context"
	"google.golang.org/grpc/grpclog"

	"github.com/wakiyamap/lnd/lnrpc"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/integration/rpctest"
	"github.com/roasbeef/btcd/rpcclient"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

// NetworkHarness is an integration testing harness for the lightning network.
// The harness by default is created with two active nodes on the network:
// Alice and Bob.
type NetworkHarness struct {
	rpcConfig rpcclient.ConnConfig
	netParams *chaincfg.Params

	// Miner is a reference to a running full node that can be used to create
	// new blocks on the network.
	Miner *rpctest.Harness

	activeNodes map[int]*HarnessNode

	nodesByPub map[string]*HarnessNode

	// Alice and Bob are the initial seeder nodes that are automatically
	// created to be the initial participants of the test network.
	Alice *HarnessNode
	Bob   *HarnessNode

	seenTxns             chan *chainhash.Hash
	bitcoinWatchRequests chan *txWatchRequest

	// Channel for transmitting stderr output from failed lightning node
	// to main process.
	lndErrorChan chan error

	quit chan struct{}

	mtx sync.Mutex
}

// NewNetworkHarness creates a new network test harness.
// TODO(roasbeef): add option to use golang's build library to a binary of the
// current repo. This will save developers from having to manually `go install`
// within the repo each time before changes
func NewNetworkHarness(r *rpctest.Harness) (*NetworkHarness, error) {
	n := NetworkHarness{
		activeNodes:          make(map[int]*HarnessNode),
		nodesByPub:           make(map[string]*HarnessNode),
		seenTxns:             make(chan *chainhash.Hash),
		bitcoinWatchRequests: make(chan *txWatchRequest),
		lndErrorChan:         make(chan error),
		netParams:            r.ActiveNet,
		Miner:                r,
		rpcConfig:            r.RPCConfig(),
		quit:                 make(chan struct{}),
	}
	go n.networkWatcher()
	return &n, nil
}

// LookUpNodeByPub queries the set of active nodes to locate a node according
// to its public key. The second value will be true if the node was found, and
// false otherwise.
func (n *NetworkHarness) LookUpNodeByPub(pubStr string) (*HarnessNode, error) {
	n.mtx.Lock()
	defer n.mtx.Unlock()

	node, ok := n.nodesByPub[pubStr]
	if !ok {
		return nil, fmt.Errorf("unable to find node")
	}

	return node, nil
}

// ProcessErrors returns a channel used for reporting any fatal process errors.
// If any of the active nodes within the harness' test network incur a fatal
// error, that error is sent over this channel.
func (n *NetworkHarness) ProcessErrors() <-chan error {
	return n.lndErrorChan
}

// fakeLogger is a fake grpclog.Logger implementation. This is used to stop
// grpc's logger from printing directly to stdout.
type fakeLogger struct{}

func (f *fakeLogger) Fatal(args ...interface{})                 {}
func (f *fakeLogger) Fatalf(format string, args ...interface{}) {}
func (f *fakeLogger) Fatalln(args ...interface{})               {}
func (f *fakeLogger) Print(args ...interface{})                 {}
func (f *fakeLogger) Printf(format string, args ...interface{}) {}
func (f *fakeLogger) Println(args ...interface{})               {}

// SetUp starts the initial seeder nodes within the test harness. The initial
// node's wallets will be funded wallets with ten 1 BTC outputs each. Finally
// rpc clients capable of communicating with the initial seeder nodes are
// created. Nodes are initialized with the given extra command line flags, which
// should be formatted properly - "--arg=value".
func (n *NetworkHarness) SetUp(lndArgs []string) error {
	// Swap out grpc's default logger with out fake logger which drops the
	// statements on the floor.
	grpclog.SetLogger(&fakeLogger{})

	// Start the initial seeder nodes within the test network, then connect
	// their respective RPC clients.
	var wg sync.WaitGroup
	errChan := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		node, err := n.NewNode(lndArgs)
		if err != nil {
			errChan <- err
			return
		}
		n.Alice = node
	}()
	go func() {
		defer wg.Done()
		node, err := n.NewNode(lndArgs)
		if err != nil {
			errChan <- err
			return
		}
		n.Bob = node
	}()
	wg.Wait()
	select {
	case err := <-errChan:
		return err
	default:
	}

	// Load up the wallets of the seeder nodes with 10 outputs of 1 BTC
	// each.
	ctxb := context.Background()
	addrReq := &lnrpc.NewAddressRequest{
		Type: lnrpc.NewAddressRequest_WITNESS_PUBKEY_HASH,
	}
	clients := []lnrpc.LightningClient{n.Alice, n.Bob}
	for _, client := range clients {
		for i := 0; i < 10; i++ {
			resp, err := client.NewAddress(ctxb, addrReq)
			if err != nil {
				return err
			}
			addr, err := btcutil.DecodeAddress(resp.Address, n.netParams)
			if err != nil {
				return err
			}
			addrScript, err := txscript.PayToAddrScript(addr)
			if err != nil {
				return err
			}

			output := &wire.TxOut{
				PkScript: addrScript,
				Value:    btcutil.SatoshiPerBitcoin,
			}
			if _, err := n.Miner.SendOutputs([]*wire.TxOut{output}, 30); err != nil {
				return err
			}
		}
	}

	// We generate several blocks in order to give the outputs created
	// above a good number of confirmations.
	if _, err := n.Miner.Node.Generate(10); err != nil {
		return err
	}

	// Finally, make a connection between both of the nodes.
	if err := n.ConnectNodes(ctxb, n.Alice, n.Bob); err != nil {
		return err
	}

	// Now block until both wallets have fully synced up.
	expectedBalance := int64(btcutil.SatoshiPerBitcoin * 10)
	balReq := &lnrpc.WalletBalanceRequest{}
	balanceTicker := time.Tick(time.Millisecond * 50)
	balanceTimeout := time.After(time.Second * 30)
out:
	for {
		select {
		case <-balanceTicker:
			aliceResp, err := n.Alice.WalletBalance(ctxb, balReq)
			if err != nil {
				return err
			}
			bobResp, err := n.Bob.WalletBalance(ctxb, balReq)
			if err != nil {
				return err
			}

			if aliceResp.ConfirmedBalance == expectedBalance &&
				bobResp.ConfirmedBalance == expectedBalance {
				break out
			}
		case <-balanceTimeout:
			return fmt.Errorf("balances not synced after deadline")
		}
	}

	return nil
}

// TearDownAll tears down all active nodes within the test lightning network.
func (n *NetworkHarness) TearDownAll() error {

	for _, node := range n.activeNodes {
		if err := n.ShutdownNode(node); err != nil {
			return err
		}
	}

	close(n.lndErrorChan)
	close(n.quit)

	return nil
}

// NewNode fully initializes a returns a new HarnessNode bound to the
// current instance of the network harness. The created node is running, but
// not yet connected to other nodes within the network.
func (n *NetworkHarness) NewNode(extraArgs []string) (*HarnessNode, error) {
	node, err := newNode(nodeConfig{
		RPCConfig: &n.rpcConfig,
		NetParams: n.netParams,
		ExtraArgs: extraArgs,
	})
	if err != nil {
		return nil, err
	}

	// Put node in activeNodes to ensure Shutdown is called even if Start
	// returns an error.
	n.mtx.Lock()
	n.activeNodes[node.NodeID] = node
	n.mtx.Unlock()

	if err := node.start(n.lndErrorChan); err != nil {
		return nil, err
	}

	// With the node started, we can now record its public key within the
	// global mapping.
	n.mtx.Lock()
	n.nodesByPub[node.PubKeyStr] = node
	n.mtx.Unlock()

	return node, nil
}

// EnsureConnected will try to connect to two nodes, returning no error if they
// are already connected. If the nodes were not connected previously, this will
// behave the same as ConnectNodes. If a pending connection request has already
// been made, the method will block until the two nodes appear in each other's
// peers list, or until the 15s timeout expires.
func (n *NetworkHarness) EnsureConnected(ctx context.Context, a, b *HarnessNode) error {
	bobInfo, err := b.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return err
	}

	req := &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{
			Pubkey: bobInfo.IdentityPubkey,
			Host:   b.cfg.P2PAddr(),
		},
	}

	_, err = a.ConnectPeer(ctx, req)
	switch {

	// Request was successful, wait for both to display the connection.
	case err == nil:

	// If we already have pending connection, we will wait until bob appears
	// in alice's peer list.
	case strings.Contains(err.Error(), "connection attempt to ") &&
		strings.Contains(err.Error(), " is pending"):

	// If the two are already connected, we return early with no error.
	case strings.Contains(err.Error(), "already connected to peer"):
		return nil

	default:
		return err
	}

	err = WaitPredicate(func() bool {
		// If node B is seen in the ListPeers response from node A,
		// then we can exit early as the connection has been fully
		// established.
		resp, err := a.ListPeers(ctx, &lnrpc.ListPeersRequest{})
		if err != nil {
			return false
		}

		for _, peer := range resp.Peers {
			if peer.PubKey == b.PubKeyStr {
				return true
			}
		}

		return false
	}, time.Second*15)
	if err != nil {
		return fmt.Errorf("peers not connected within 15 seconds")
	}

	return nil
}

// ConnectNodes establishes an encrypted+authenticated p2p connection from node
// a towards node b. The function will return a non-nil error if the connection
// was unable to be established.
//
// NOTE: This function may block for up to 15-seconds as it will not return
// until the new connection is detected as being known to both nodes.
func (n *NetworkHarness) ConnectNodes(ctx context.Context, a, b *HarnessNode) error {
	bobInfo, err := b.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return err
	}

	req := &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{
			Pubkey: bobInfo.IdentityPubkey,
			Host:   b.cfg.P2PAddr(),
		},
	}
	if _, err := a.ConnectPeer(ctx, req); err != nil {
		return err
	}

	err = WaitPredicate(func() bool {
		// If node B is seen in the ListPeers response from node A,
		// then we can exit early as the connection has been fully
		// established.
		resp, err := a.ListPeers(ctx, &lnrpc.ListPeersRequest{})
		if err != nil {
			return false
		}

		for _, peer := range resp.Peers {
			if peer.PubKey == b.PubKeyStr {
				return true
			}
		}

		return false
	}, time.Second*15)
	if err != nil {
		return fmt.Errorf("peers not connected within 15 seconds")
	}

	return nil
}

// DisconnectNodes disconnects node a from node b by sending RPC message
// from a node to b node
func (n *NetworkHarness) DisconnectNodes(ctx context.Context, a, b *HarnessNode) error {
	bobInfo, err := b.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return err
	}

	req := &lnrpc.DisconnectPeerRequest{
		PubKey: bobInfo.IdentityPubkey,
	}

	if _, err := a.DisconnectPeer(ctx, req); err != nil {
		return err
	}

	return nil
}

// RestartNode  attempts to restart a lightning node by shutting it down
// cleanly, then restarting the process. This function is fully blocking. Upon
// restart, the RPC connection to the node will be re-attempted, continuing iff
// the connection attempt is successful. If the callback parameter is non-nil,
// then the function will be executed after the node shuts down, but *before*
// the process has been started up again.
//
// This method can be useful when testing edge cases such as a node broadcast
// and invalidated prior state, or persistent state recovery, simulating node
// crashes, etc.
func (n *NetworkHarness) RestartNode(node *HarnessNode, callback func() error) error {
	if err := node.stop(); err != nil {
		return err
	}

	if callback != nil {
		if err := callback(); err != nil {
			return err
		}
	}

	return node.start(n.lndErrorChan)
}

// ShutdownNode stops an active lnd process and returns when the process has
// exited and any temporary directories have been cleaned up.
func (n *NetworkHarness) ShutdownNode(node *HarnessNode) error {
	if err := node.shutdown(); err != nil {
		return err
	}

	delete(n.activeNodes, node.NodeID)
	return nil
}

// TODO(roasbeef): add a WithChannel higher-order function?
//  * python-like context manager w.r.t using a channel within a test
//  * possibly  adds more funds to the target wallet if the funds are not
//    enough

// txWatchRequest encapsulates a request to the harness' Bitcoin network
// watcher to dispatch a notification once a transaction with the target txid
// is seen within the test network.
type txWatchRequest struct {
	txid      chainhash.Hash
	eventChan chan struct{}
}

// bitcoinNetworkWatcher is a goroutine which accepts async notification
// requests for the broadcast of a target transaction, and then dispatches the
// transaction once its seen on the Bitcoin network.
func (n *NetworkHarness) networkWatcher() {
	seenTxns := make(map[chainhash.Hash]struct{})
	clients := make(map[chainhash.Hash][]chan struct{})

	for {

		select {
		case <-n.quit:
			return

		case req := <-n.bitcoinWatchRequests:
			// If we've already seen this transaction, then
			// immediately dispatch the request. Otherwise, append
			// to the list of clients who are watching for the
			// broadcast of this transaction.
			if _, ok := seenTxns[req.txid]; ok {
				close(req.eventChan)
			} else {
				clients[req.txid] = append(clients[req.txid], req.eventChan)
			}
		case txid := <-n.seenTxns:
			// Add this txid to our set of "seen" transactions. So
			// we're able to dispatch any notifications for this
			// txid which arrive *after* it's seen within the
			// network.
			seenTxns[*txid] = struct{}{}

			// If there isn't a registered notification for this
			// transaction then ignore it.
			txClients, ok := clients[*txid]
			if !ok {
				continue
			}

			// Otherwise, dispatch the notification to all clients,
			// cleaning up the now un-needed state.
			for _, client := range txClients {
				close(client)
			}
			delete(clients, *txid)
		}
	}
}

// OnTxAccepted is a callback to be called each time a new transaction has been
// broadcast on the network.
func (n *NetworkHarness) OnTxAccepted(hash *chainhash.Hash) {
	select {
	case n.seenTxns <- hash:
	case <-n.quit:
		return
	}
}

// WaitForTxBroadcast blocks until the target txid is seen on the network. If
// the transaction isn't seen within the network before the passed timeout,
// then an error is returned.
// TODO(roasbeef): add another method which creates queue of all seen transactions
func (n *NetworkHarness) WaitForTxBroadcast(ctx context.Context, txid chainhash.Hash) error {
	// Return immediately if harness has been torn down.
	select {
	case <-n.quit:
		return fmt.Errorf("NetworkHarness has been torn down")
	default:
	}

	eventChan := make(chan struct{})

	n.bitcoinWatchRequests <- &txWatchRequest{
		txid:      txid,
		eventChan: eventChan,
	}

	select {
	case <-eventChan:
		return nil
	case <-n.quit:
		return fmt.Errorf("NetworkHarness has been torn down")
	case <-ctx.Done():
		return fmt.Errorf("tx not seen before context timeout")
	}
}

// OpenChannel attempts to open a channel between srcNode and destNode with the
// passed channel funding parameters. If the passed context has a timeout, then
// if the timeout is reached before the channel pending notification is
// received, an error is returned.
func (n *NetworkHarness) OpenChannel(ctx context.Context,
	srcNode, destNode *HarnessNode, amt btcutil.Amount,
	pushAmt btcutil.Amount, private bool) (lnrpc.Lightning_OpenChannelClient, error) {

	// Wait until srcNode and destNode have the latest chain synced.
	// Otherwise, we may run into a check within the funding manager that
	// prevents any funding workflows from being kicked off if the chain
	// isn't yet synced.
	if err := srcNode.WaitForBlockchainSync(ctx); err != nil {
		return nil, fmt.Errorf("Unable to sync srcNode chain: %v", err)
	}
	if err := destNode.WaitForBlockchainSync(ctx); err != nil {
		return nil, fmt.Errorf("Unable to sync destNode chain: %v", err)
	}

	openReq := &lnrpc.OpenChannelRequest{
		NodePubkey:         destNode.PubKey[:],
		LocalFundingAmount: int64(amt),
		PushSat:            int64(pushAmt),
		Private:            private,
	}

	respStream, err := srcNode.OpenChannel(ctx, openReq)
	if err != nil {
		return nil, fmt.Errorf("unable to open channel between "+
			"alice and bob: %v", err)
	}

	chanOpen := make(chan struct{})
	errChan := make(chan error)
	go func() {
		// Consume the "channel pending" update. This waits until the node
		// notifies us that the final message in the channel funding workflow
		// has been sent to the remote node.
		resp, err := respStream.Recv()
		if err != nil {
			errChan <- err
			return
		}
		if _, ok := resp.Update.(*lnrpc.OpenStatusUpdate_ChanPending); !ok {
			errChan <- fmt.Errorf("expected channel pending update, "+
				"instead got %v", resp)
			return
		}

		close(chanOpen)
	}()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("timeout reached before chan pending "+
			"update sent: %v", err)
	case err := <-errChan:
		return nil, err
	case <-chanOpen:
		return respStream, nil
	}
}

// OpenPendingChannel attempts to open a channel between srcNode and destNode with the
// passed channel funding parameters. If the passed context has a timeout, then
// if the timeout is reached before the channel pending notification is
// received, an error is returned.
func (n *NetworkHarness) OpenPendingChannel(ctx context.Context,
	srcNode, destNode *HarnessNode, amt btcutil.Amount,
	pushAmt btcutil.Amount) (*lnrpc.PendingUpdate, error) {

	// Wait until srcNode and destNode have blockchain synced
	if err := srcNode.WaitForBlockchainSync(ctx); err != nil {
		return nil, fmt.Errorf("Unable to sync srcNode chain: %v", err)
	}
	if err := destNode.WaitForBlockchainSync(ctx); err != nil {
		return nil, fmt.Errorf("Unable to sync destNode chain: %v", err)
	}

	openReq := &lnrpc.OpenChannelRequest{
		NodePubkey:         destNode.PubKey[:],
		LocalFundingAmount: int64(amt),
		PushSat:            int64(pushAmt),
		Private:            false,
	}

	respStream, err := srcNode.OpenChannel(ctx, openReq)
	if err != nil {
		return nil, fmt.Errorf("unable to open channel between "+
			"alice and bob: %v", err)
	}

	chanPending := make(chan *lnrpc.PendingUpdate)
	errChan := make(chan error)
	go func() {
		// Consume the "channel pending" update. This waits until the node
		// notifies us that the final message in the channel funding workflow
		// has been sent to the remote node.
		resp, err := respStream.Recv()
		if err != nil {
			errChan <- err
			return
		}
		pendingResp, ok := resp.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
		if !ok {
			errChan <- fmt.Errorf("expected channel pending update, "+
				"instead got %v", resp)
			return
		}

		chanPending <- pendingResp.ChanPending
	}()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("timeout reached before chan pending " +
			"update sent")
	case err := <-errChan:
		return nil, err
	case pendingChan := <-chanPending:
		return pendingChan, nil
	}
}

// WaitForChannelOpen waits for a notification that a channel is open by
// consuming a message from the past open channel stream. If the passed context
// has a timeout, then if the timeout is reached before the channel has been
// opened, then an error is returned.
func (n *NetworkHarness) WaitForChannelOpen(ctx context.Context,
	openChanStream lnrpc.Lightning_OpenChannelClient) (*lnrpc.ChannelPoint, error) {

	errChan := make(chan error)
	respChan := make(chan *lnrpc.ChannelPoint)
	go func() {
		resp, err := openChanStream.Recv()
		if err != nil {
			errChan <- fmt.Errorf("unable to read rpc resp: %v", err)
			return
		}
		fundingResp, ok := resp.Update.(*lnrpc.OpenStatusUpdate_ChanOpen)
		if !ok {
			errChan <- fmt.Errorf("expected channel open update, "+
				"instead got %v", resp)
			return
		}

		respChan <- fundingResp.ChanOpen.ChannelPoint
	}()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("timeout reached while waiting for " +
			"channel open")
	case err := <-errChan:
		return nil, err
	case chanPoint := <-respChan:
		return chanPoint, nil
	}
}

// CloseChannel close channel attempts to close the channel indicated by the
// passed channel point, initiated by the passed lnNode. If the passed context
// has a timeout, then if the timeout is reached before the channel close is
// pending, then an error is returned.
func (n *NetworkHarness) CloseChannel(ctx context.Context,
	lnNode *HarnessNode, cp *lnrpc.ChannelPoint,
	force bool) (lnrpc.Lightning_CloseChannelClient, *chainhash.Hash, error) {

	// Create a channel outpoint that we can use to compare to channels
	// from the ListChannelsResponse.
	txidHash, err := getChanPointFundingTxid(cp)
	if err != nil {
		return nil, nil, err
	}
	fundingTxID, err := chainhash.NewHash(txidHash)
	if err != nil {
		return nil, nil, err
	}
	chanPoint := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: cp.OutputIndex,
	}

	// We'll wait for *both* nodes to read the channel as active if we're
	// performing a cooperative channel closure.
	if !force {
		timeout := time.Second * 15
		listReq := &lnrpc.ListChannelsRequest{}

		// We define two helper functions, one two locate a particular
		// channel, and the other to check if a channel is active or
		// not.
		filterChannel := func(node *HarnessNode,
			op wire.OutPoint) (*lnrpc.Channel, error) {
			listResp, err := node.ListChannels(ctx, listReq)
			if err != nil {
				return nil, err
			}

			for _, c := range listResp.Channels {
				if c.ChannelPoint == op.String() {
					return c, nil
				}
			}

			return nil, fmt.Errorf("unable to find channel")
		}
		activeChanPredicate := func(node *HarnessNode) func() bool {
			return func() bool {
				channel, err := filterChannel(node, chanPoint)
				if err != nil {
				}

				return channel.Active
			}
		}

		// Next, we'll fetch the target channel in order to get the
		// harness node that will be receiving the channel close request.
		targetChan, err := filterChannel(lnNode, chanPoint)
		if err != nil {
			return nil, nil, err
		}
		receivingNode, err := n.LookUpNodeByPub(targetChan.RemotePubkey)
		if err != nil {
			return nil, nil, err
		}

		// Before proceeding, we'll ensure that the channel is active
		// for both nodes.
		err = WaitPredicate(activeChanPredicate(lnNode), timeout)
		if err != nil {
			return nil, nil, fmt.Errorf("channel of closing " +
				"node not active in time")
		}
		err = WaitPredicate(activeChanPredicate(receivingNode), timeout)
		if err != nil {
			return nil, nil, fmt.Errorf("channel of receiving " +
				"node not active in time")
		}
	}

	closeReq := &lnrpc.CloseChannelRequest{
		ChannelPoint: cp,
		Force:        force,
	}
	closeRespStream, err := lnNode.CloseChannel(ctx, closeReq)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to close channel: %v", err)
	}

	errChan := make(chan error)
	fin := make(chan *chainhash.Hash)
	go func() {
		// Consume the "channel close" update in order to wait for the closing
		// transaction to be broadcast, then wait for the closing tx to be seen
		// within the network.
		closeResp, err := closeRespStream.Recv()
		if err != nil {
			errChan <- err
			return
		}
		pendingClose, ok := closeResp.Update.(*lnrpc.CloseStatusUpdate_ClosePending)
		if !ok {
			errChan <- fmt.Errorf("expected channel close update, "+
				"instead got %v", pendingClose)
			return
		}

		closeTxid, err := chainhash.NewHash(pendingClose.ClosePending.Txid)
		if err != nil {
			errChan <- err
			return
		}
		if err := n.WaitForTxBroadcast(ctx, *closeTxid); err != nil {
			errChan <- err
			return
		}
		fin <- closeTxid
	}()

	// Wait until either the deadline for the context expires, an error
	// occurs, or the channel close update is received.
	select {
	case <-ctx.Done():
		return nil, nil, fmt.Errorf("timeout reached before channel close " +
			"initiated")
	case err := <-errChan:
		return nil, nil, err
	case closeTxid := <-fin:
		return closeRespStream, closeTxid, nil
	}
}

// WaitForChannelClose waits for a notification from the passed channel close
// stream that the node has deemed the channel has been fully closed. If the
// passed context has a timeout, then if the timeout is reached before the
// notification is received then an error is returned.
func (n *NetworkHarness) WaitForChannelClose(ctx context.Context,
	closeChanStream lnrpc.Lightning_CloseChannelClient) (*chainhash.Hash, error) {

	errChan := make(chan error)
	updateChan := make(chan *lnrpc.CloseStatusUpdate_ChanClose)
	go func() {
		closeResp, err := closeChanStream.Recv()
		if err != nil {
			errChan <- err
			return
		}

		closeFin, ok := closeResp.Update.(*lnrpc.CloseStatusUpdate_ChanClose)
		if !ok {
			errChan <- fmt.Errorf("expected channel close update, "+
				"instead got %v", closeFin)
			return
		}

		updateChan <- closeFin
	}()

	// Wait until either the deadline for the context expires, an error
	// occurs, or the channel close update is received.
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("timeout reached before update sent")
	case err := <-errChan:
		return nil, err
	case update := <-updateChan:
		return chainhash.NewHash(update.ChanClose.ClosingTxid)
	}
}

// AssertChannelExists asserts that an active channel identified by
// channelPoint is known to exist from the point-of-view of node..
func (n *NetworkHarness) AssertChannelExists(ctx context.Context,
	node *HarnessNode, chanPoint *wire.OutPoint) error {

	req := &lnrpc.ListChannelsRequest{}

	var predErr error
	pred := func() bool {
		resp, err := node.ListChannels(ctx, req)
		if err != nil {
			predErr = fmt.Errorf("unable fetch node's channels: %v", err)
			return false
		}

		for _, channel := range resp.Channels {
			if channel.ChannelPoint == chanPoint.String() {
				return true
			}

		}
		return false
	}

	if err := WaitPredicate(pred, time.Second*15); err != nil {
		return fmt.Errorf("channel not found: %v", predErr)
	}

	return nil
}

// WaitPredicate is a helper test function that will wait for a timeout period
// of time until the passed predicate returns true. This function is helpful as
// timing doesn't always line up well when running integration tests with
// several running lnd nodes. This function gives callers a way to assert that
// some property is upheld within a particular time frame.
func WaitPredicate(pred func() bool, timeout time.Duration) error {
	exitTimer := time.After(timeout)
	for {
		select {
		case <-exitTimer:
			return fmt.Errorf("predicate not satisfied after time out")
		default:
		}

		if pred() {
			return nil
		}
	}
}

// WaitInvariant is a helper test function that will wait for a timeout period
// of time, verifying that a statement remains true for the entire duration.
// This function is helpful as timing doesn't always line up well when running
// integration tests with several running lnd nodes. This function gives callers
// a way to assert that some property is maintained over a particular time
// frame.
func WaitInvariant(statement func() bool, timeout time.Duration) error {
	const pollInterval = 20 * time.Millisecond

	exitTimer := time.After(timeout)
	for {
		<-time.After(pollInterval)

		// Fail if the invariant is broken while polling.
		if !statement() {
			return fmt.Errorf("invariant broken before time out")
		}

		select {
		case <-exitTimer:
			return nil
		default:
		}
	}
}

// DumpLogs reads the current logs generated by the passed node, and returns
// the logs as a single string. This function is useful for examining the logs
// of a particular node in the case of a test failure.
// Logs from lightning node being generated with delay - you should
// add time.Sleep() in order to get all logs.
func (n *NetworkHarness) DumpLogs(node *HarnessNode) (string, error) {
	logFile := fmt.Sprintf("%v/simnet/lnd.log", node.cfg.LogDir)

	buf, err := ioutil.ReadFile(logFile)
	if err != nil {
		return "", err
	}

	return string(buf), nil
}

// SendCoins attempts to send amt satoshis from the internal mining node to the
// targeted lightning node.
func (n *NetworkHarness) SendCoins(ctx context.Context, amt btcutil.Amount,
	target *HarnessNode) error {

	balReq := &lnrpc.WalletBalanceRequest{}
	initialBalance, err := target.WalletBalance(ctx, balReq)
	if err != nil {
		return err
	}

	// First, obtain an address from the target lightning node, preferring
	// to receive a p2wkh address s.t the output can immediately be used as
	// an input to a funding transaction.
	addrReq := &lnrpc.NewAddressRequest{
		Type: lnrpc.NewAddressRequest_WITNESS_PUBKEY_HASH,
	}
	resp, err := target.NewAddress(ctx, addrReq)
	if err != nil {
		return err
	}
	addr, err := btcutil.DecodeAddress(resp.Address, n.netParams)
	if err != nil {
		return err
	}
	addrScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return err
	}

	// Generate a transaction which creates an output to the target
	// pkScript of the desired amount.
	output := &wire.TxOut{
		PkScript: addrScript,
		Value:    int64(amt),
	}
	if _, err := n.Miner.SendOutputs([]*wire.TxOut{output}, 30); err != nil {
		return err
	}

	// Finally, generate 6 new blocks to ensure the output gains a
	// sufficient number of confirmations.
	if _, err := n.Miner.Node.Generate(6); err != nil {
		return err
	}

	// Pause until the nodes current wallet balances reflects the amount
	// sent to it above.
	// TODO(roasbeef): factor out into helper func
	balanceTicker := time.Tick(time.Millisecond * 50)
	balanceTimeout := time.After(time.Second * 30)
	for {
		select {
		case <-balanceTicker:
			currentBal, err := target.WalletBalance(ctx, balReq)
			if err != nil {
				return err
			}

			if currentBal.ConfirmedBalance == initialBalance.ConfirmedBalance+int64(amt) {
				return nil
			}
		case <-balanceTimeout:
			return fmt.Errorf("balances not synced after deadline")
		}
	}
}
