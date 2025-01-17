package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/hive/hivesim"
)

var (
	// This is the account that sends vault funding transactions.
	vaultAccountAddr = common.HexToAddress("0xcf49fda3be353c69b41ed96333cd24302da4556f")
	vaultKey, _      = crypto.HexToECDSA("63b508a03c3b5937ceb903af8b1b0c191012ef6eb7e9c3fb7afa94e5d214d376")
)

type TestTransactionType int

const (
	UnspecifiedTransactionType TestTransactionType = iota
	LegacyTxOnly
	DynamicFeeTxOnly
)

// TestEnv is the environment of a single test.
type TestEnv struct {
	*hivesim.T
	TestName string
	Client   *hivesim.Client

	// RPC Clients
	RPC        *rpc.Client
	Eth        *ethclient.Client
	Engine     *EngineClient
	TestEngine *TestEngineClient
	TestEth    *TestEthClient

	// Consensus Layer Mocker Instance
	CLMock *CLMocker

	// Global test timeout
	Timeout <-chan time.Time

	// Client parameters used to launch the default client
	ClientParams hivesim.Params
	ClientFiles  hivesim.Params

	// This tracks the account nonce of the vault account.
	nonce uint64

	// Sets the type of transactions to use during the test
	TestTransactionType TestTransactionType

	// This holds most recent context created by the Ctx method.
	// Every time Ctx is called, it creates a new context with the default
	// timeout and cancels the previous one.
	lastCtx    context.Context
	lastCancel context.CancelFunc
	syncCancel context.CancelFunc
}

func RunTest(testName string, ttd *big.Int, slotsToSafe *big.Int, slotsToFinalized *big.Int, timeout time.Duration, t *hivesim.T, c *hivesim.Client, fn func(*TestEnv), cParams hivesim.Params, cFiles hivesim.Params, testTransactionType TestTransactionType) {
	// Setup the CL Mocker for this test
	clMocker := NewCLMocker(t, slotsToSafe, slotsToFinalized)
	// Defer closing all clients
	defer func() {
		clMocker.CloseClients()
	}()

	// Add main client to CLMocker
	clMocker.AddEngineClient(t, c, ttd)

	// This sets up debug logging of the requests and responses.
	client := &http.Client{
		Transport: &loggingRoundTrip{
			t:     t,
			hc:    c,
			inner: http.DefaultTransport,
		},
	}

	// Create Engine client from main hivesim.Client to be used by tests
	ec := NewEngineClient(t, c, ttd)
	defer ec.Close()

	rpcClient, _ := rpc.DialHTTPWithClient(fmt.Sprintf("http://%v:%v/", c.IP, EthPortHTTP), client)
	defer rpcClient.Close()
	env := &TestEnv{
		T:                   t,
		TestName:            testName,
		Client:              c,
		RPC:                 rpcClient,
		Eth:                 ethclient.NewClient(rpcClient),
		Engine:              ec,
		CLMock:              clMocker,
		ClientParams:        cParams,
		ClientFiles:         cFiles,
		TestTransactionType: testTransactionType,
	}
	env.TestEngine = NewTestEngineClient(env, ec)
	env.TestEth = NewTestEthClient(env, env.Eth)

	// Defer closing the last context
	defer func() {
		if env.lastCtx != nil {
			env.lastCancel()
		}
	}()

	// Create test end channel and defer closing it
	testend := make(chan interface{})
	defer func() { close(testend) }()

	// Start thread to wait for client to be synced to the latest PoS block
	defer func() {
		if env.syncCancel != nil {
			env.syncCancel()
		}
	}()

	// Before running the test, make sure Eth and Engine ports are open for the client
	if err := CheckEthEngineLive(c); err != nil {
		t.Fatalf("FAIL (%s): Ports were never open for client: %v", env.TestName, err)
	}

	// Setup timeouts
	env.Timeout = time.After(timeout)
	clMocker.Timeout = time.After(timeout)

	// Defer producing one last block to verify Execution client did not break after the test
	defer func() {
		// Only run if the TTD was reached during test, and test had not failed at this point.
		if clMocker.TTDReached && !t.Failed() {
			clMocker.produceSingleBlock(BlockProcessCallbacks{})
		}
	}()

	// Run the test
	fn(env)
}

func (t *TestEnv) MainTTD() *big.Int {
	return t.Engine.TerminalTotalDifficulty
}

func (t *TestEnv) StartClient(clientType string, params hivesim.Params, ttd *big.Int) (*hivesim.Client, *EngineClient, error) {
	c := t.T.StartClient(clientType, params, hivesim.WithStaticFiles(t.ClientFiles))
	if err := CheckEthEngineLive(c); err != nil {
		return nil, nil, fmt.Errorf("Engine/Eth ports were never open for client: %v", err)
	}
	ec := NewEngineClient(t.T, c, ttd)
	return c, ec, nil
}

func CheckEthEngineLive(c *hivesim.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	var (
		ticker = time.NewTicker(100 * time.Millisecond)
		dialer net.Dialer
	)
	defer ticker.Stop()
	for _, checkport := range []int{EthPortHTTP, EnginePortHTTP} {
		addr := fmt.Sprintf("%s:%d", c.IP, checkport)
	portcheckloop:
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				conn, err := dialer.DialContext(ctx, "tcp", addr)
				if err == nil {
					conn.Close()
					break portcheckloop
				}
			}
		}
	}
	return nil
}

func (t *TestEnv) makeNextTransaction(recipient *common.Address, gasLimit uint64, amount *big.Int, payload []byte) *types.Transaction {
	var (
		newTxData types.TxData
		txType    string
	)
	var txTypeToUse int
	switch t.TestTransactionType {
	case UnspecifiedTransactionType:
		// Test case has no specific type of transaction to use.
		// Select the type of tx based on the nonce.
		switch t.nonce % 2 {
		case 0:
			txTypeToUse = types.LegacyTxType
		case 1:
			txTypeToUse = types.DynamicFeeTxType
		}
	case LegacyTxOnly:
		txTypeToUse = types.LegacyTxType
	case DynamicFeeTxOnly:
		txTypeToUse = types.DynamicFeeTxType
	}

	// Build the transaction depending on the specified type
	switch txTypeToUse {
	case types.LegacyTxType:
		newTxData = &types.LegacyTx{
			Nonce:    t.nonce,
			To:       recipient,
			Value:    amount,
			Gas:      gasLimit,
			GasPrice: gasPrice,
			Data:     payload,
		}
		txType = "Legacy"
	case types.DynamicFeeTxType:
		gasFeeCap := new(big.Int).Set(gasPrice)
		gasTipCap := new(big.Int).Set(gasTipPrice)
		newTxData = &types.DynamicFeeTx{
			Nonce:     t.nonce,
			Gas:       gasLimit,
			GasTipCap: gasTipCap,
			GasFeeCap: gasFeeCap,
			To:        recipient,
			Value:     amount,
			Data:      payload,
		}
		txType = "DynamicFeeTx"
	}

	tx := types.NewTx(newTxData)
	signedTx, err := types.SignTx(tx, types.NewLondonSigner(chainID), vaultKey)
	if err != nil {
		t.Fatalf("FAIL (%s): could not sign new tx: %v", t.TestName, err)
	}
	t.Logf("INFO (%s): Built next transaction: hash=%s, nonce=%d, recipient=%s, type=%s", t.TestName, signedTx.Hash(), t.nonce, recipient, txType)
	t.nonce++
	return signedTx
}

func (t *TestEnv) sendNextTransaction(node *EngineClient, recipient common.Address, amount *big.Int, payload []byte) *types.Transaction {
	tx := t.makeNextTransaction(&recipient, 75000, amount, payload)
	for {
		err := node.Eth.SendTransaction(node.Ctx(), tx)
		if err == nil {
			return tx
		}
		select {
		case <-time.After(time.Second):
		case <-t.Timeout:
			t.Fatalf("FAIL (%s): Timeout while trying to send transaction: %v", t.TestName, err)
		}
	}
}

// Method that attempts to create a contract filled with zeros without going over the specified gasLimit
func (t *TestEnv) makeNextBigContractTransaction(gasLimit uint64) *types.Transaction {
	// Total GAS: Gtransaction == 21000, Gcreate == 32000, Gcodedeposit == 200
	contractLength := uint64(0)
	if gasLimit > (21000 + 32000) {
		contractLength = (gasLimit - 21000 - 32000) / 200
		if contractLength >= 1 {
			// Reduce by 1 to guarantee using less gas than requested
			contractLength -= 1
		}
	}
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, contractLength)

	initCode := []byte{
		0x67, // PUSH8
	}
	initCode = append(initCode, buf...) // Size of the contract in byte length
	initCode = append(initCode, 0x38)   // CODESIZE == 0x00
	initCode = append(initCode, 0xF3)   // RETURN(offset, length)

	return t.makeNextTransaction(nil, gasLimit, big0, initCode)
}

func (t *TestEnv) sendNextBigContractTransaction(sender *EngineClient, gasLimit uint64) *types.Transaction {
	tx := t.makeNextBigContractTransaction(gasLimit)
	for {
		err := sender.Eth.SendTransaction(sender.Ctx(), tx)
		if err == nil {
			return tx
		}
		select {
		case <-time.After(time.Second):
		case <-t.Timeout:
			t.Fatalf("FAIL (%s): Timeout while trying to send transaction: %v", t.TestName, err)
		}
	}
}

// Verify that the client progresses after a certain PoW block still in PoW mode
func (t *TestEnv) verifyPoWProgress(lastBlockHash common.Hash) {
	// Get the block number first
	lb, err := t.Eth.BlockByHash(t.Ctx(), lastBlockHash)
	if err != nil {
		t.Fatalf("FAIL (%s): Unable to fetch block: %v", t.TestName, err)
	}
	nextNum := lb.Number().Int64() + 1
	for {
		nh, err := t.Eth.HeaderByNumber(t.Ctx(), big.NewInt(nextNum))
		if err == nil {
			// Chain has progressed, check that the next block is also PoW
			// Difficulty must NOT be zero
			if nh.Difficulty.Cmp(big0) == 0 {
				t.Fatalf("FAIL (%s): Expected PoW chain to progress in PoW mode, but following block difficulty==%v", t.TestName, nh.Difficulty)
			}
			// Chain is still PoW/Clique
			return
		}
		t.Logf("INFO (%s): Error getting block, will try again: %v", t.TestName, err)
		select {
		case <-t.Timeout:
			t.Fatalf("FAIL (%s): Timeout while waiting for PoW chain to progress", t.TestName)
		case <-time.After(time.Second):
		}
	}
}

// CallContext is a helper method that forwards a raw RPC request to
// the underlying RPC client. This can be used to call RPC methods
// that are not supported by the ethclient.Client.
func (t *TestEnv) CallContext(ctx context.Context, result interface{}, method string, args ...interface{}) error {
	return t.RPC.CallContext(ctx, result, method, args...)
}

// Ctx returns a context with the default timeout.
// For subsequent calls to Ctx, it also cancels the previous context.
func (t *TestEnv) Ctx() context.Context {
	if t.lastCtx != nil {
		t.lastCancel()
	}
	t.lastCtx, t.lastCancel = context.WithTimeout(context.Background(), rpcTimeout)
	return t.lastCtx
}
