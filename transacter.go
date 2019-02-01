package main

import (
	"time"
	"sync"
	"math/rand"
	"net/http"
	"net/url"
	"net"
	"os"
	"encoding/json"
	"fmt"
	"github.com/spf13/viper"
	"github.com/QOSGroup/qbase/txs"
	"github.com/QOSGroup/qbase/types"
	"github.com/tendermint/tendermint/rpc/lib/types"
	"github.com/gorilla/websocket"
	"github.com/tendermint/tendermint/libs/log"
	"github.com/pkg/errors"
	"github.com/QOSGroup/qos/module/transfer"
 	clictx "github.com/QOSGroup/qbase/client/context"
	cflags "github.com/QOSGroup/qbase/client/types"
	tmrpc "github.com/tendermint/tendermint/rpc/client"
	transfertypes "github.com/QOSGroup/qos/module/transfer/types"
	"github.com/QOSGroup/qbase/client/keys"
	"bytes"
	"github.com/tendermint/tendermint/crypto"
	"github.com/QOSGroup/qos/app"
	"github.com/QOSGroup/qbase/client/account"
)

const (
	sendTimeout = 10 * time.Second
	// see https://github.com/tendermint/tendermint/blob/master/rpc/lib/server/handlers.go
	pingPeriod = (30 * 9 / 10) * time.Second
)

type transacter struct {
	Target            string
	Rate              int
	Size              int
	Connections       int
	BroadcastTxMethod string

	conns       []*websocket.Conn
	connsBroken []bool
	startingWg  sync.WaitGroup
	endingWg    sync.WaitGroup
	stopped     bool

	logger log.Logger
}

func newTransacter(target string, connections, rate int, size int, broadcastTxMethod string) *transacter {
	return &transacter{
		Target:            target,
		Rate:              rate,
		Size:              size,
		Connections:       connections,
		BroadcastTxMethod: broadcastTxMethod,
		conns:             make([]*websocket.Conn, connections),
		connsBroken:       make([]bool, connections),
		logger:            log.NewNopLogger(),
	}
}

// SetLogger lets you set your own logger
func (t *transacter) SetLogger(l log.Logger) {
	t.logger = l
}

func (t *transacter) Start() error {
	t.stopped = false

	rand.Seed(time.Now().Unix())

	for i := 0; i < t.Connections; i++ {
		c, _, err := connect(t.Target)
		if err != nil {
			return err
		}
		t.conns[i] = c
	}

	t.startingWg.Add(t.Connections)
	t.endingWg.Add(2 * t.Connections)
	for i := 0; i < t.Connections; i++ {
		go t.sendLoop(i)
		go t.receiveLoop(i)
	}

	t.startingWg.Wait()

	return nil
}

func connect(host string) (*websocket.Conn, *http.Response, error) {
	u := url.URL{Scheme: "ws", Host: host, Path: "/websocket"}
	return websocket.DefaultDialer.Dial(u.String(), nil)
}

func (t *transacter) sendLoop(connIndex int) {
	started := false
	// Close the starting waitgroup, in the event that this fails to start
	defer func() {
		if !started {
			t.startingWg.Done()
		}
	}()
	c := t.conns[connIndex]

	c.SetPingHandler(func(message string) error {
		err := c.WriteControl(websocket.PongMessage, []byte(message), time.Now().Add(sendTimeout))
		if err == websocket.ErrCloseSent {
			return nil
		} else if e, ok := err.(net.Error); ok && e.Temporary() {
			return nil
		}
		return err
	})

	logger := t.logger.With("addr", c.RemoteAddr())

	pingsTicker := time.NewTicker(pingPeriod)
	txsTicker := time.NewTicker(1 * time.Second)
	defer func() {
		pingsTicker.Stop()
		txsTicker.Stop()
		t.endingWg.Done()
	}()

	client := tmrpc.NewHTTP("localhost:26657", "/websocket")
	ctx := clictx.NewCLIContext().WithCodec(app.MakeCodec()).WithClient(client)
	singerNonce := getSignerNonce(ctx)

	var txNumber = 0
	for {
		select {
		case <-txsTicker.C:
			startTime := time.Now()
			endTime := startTime.Add(time.Second)
			numTxSent := t.Rate
			if !started {
				t.startingWg.Done()
				started = true
			}
			now := time.Now()
			fmt.Println("time RIGHT NOW: ", now)

			for i := 0; i < t.Rate; i++ {
				txNumber++
				//// update tx number of the tx, and the corresponding hex
				fmt.Println("txNumber: ", txNumber)
				BroadcastTx(ctx, int64(txNumber), singerNonce)
				paramsJSON, err := json.Marshal(map[string]interface{}{"tx": txNumber})

				if err != nil {
					fmt.Printf("failed to encode params: %v\n", err)
					os.Exit(1)
				}
				rawParamsJSON := json.RawMessage(paramsJSON)

				c.SetWriteDeadline(now.Add(sendTimeout))
				err = c.WriteJSON(rpctypes.RPCRequest{
					JSONRPC: "2.0",
					ID:      rpctypes.JSONRPCStringID("qos-bench"),
					Method:  t.BroadcastTxMethod,
					Params:  rawParamsJSON,
				})
				if err != nil {
					err = errors.Wrap(err,
						fmt.Sprintf("txs send failed on connection #%d", connIndex))
					t.connsBroken[connIndex] = true
					logger.Error(err.Error())
					return
				}

				// cache the time.Now() reads to save time.
				if i%5 == 0 {
					now = time.Now()
					if now.After(endTime) {
						// Plus one accounts for sending this tx
						numTxSent = i + 1
						break
					}
				}
			}

			timeToSend := time.Since(startTime)
			logger.Info(fmt.Sprintf("sent %d transactions", numTxSent), "took", timeToSend)
			if timeToSend < 1*time.Second {
				sleepTime := time.Second - timeToSend
				logger.Debug(fmt.Sprintf("connection #%d is sleeping for %f seconds", connIndex, sleepTime.Seconds()))
				time.Sleep(sleepTime)
			}

		case <-pingsTicker.C:
			// go-rpc server closes the connection in the absence of pings
			c.SetWriteDeadline(time.Now().Add(sendTimeout))
			if err := c.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				err = errors.Wrap(err,
					fmt.Sprintf("failed to write ping message on conn #%d", connIndex))
				logger.Error(err.Error())
				t.connsBroken[connIndex] = true
			}
		}

		if t.stopped {
			// To cleanly close a connection, a client should send a close
			// frame and wait for the server to close the connection.
			c.SetWriteDeadline(time.Now().Add(sendTimeout))
			err := c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			if err != nil {
				err = errors.Wrap(err,
					fmt.Sprintf("failed to write close message on conn #%d", connIndex))
				logger.Error(err.Error())
				t.connsBroken[connIndex] = true
			}

			return
		}
	}
}

func getSignerNonce(ctx clictx.CLIContext) (map[string]int64) {
	keybase, _ := keys.GetKeyBaseFromDir(ctx, "/Users/shen/.qoscli")
	var signerNonce = make(map[string]int64)
	infos, _ := keybase.List()
	for _, info := range infos {
		nonce, _ := getDefaultAccountNonce(ctx, info.GetAddress().Bytes())
		signerNonce[info.GetName()] = nonce
	}

	return signerNonce
}

func BroadcastTx(ctx clictx.CLIContext, txNumber int64, signerNonce map[string]int64) ([]byte, error) {
	maxGas := viper.GetInt64(cflags.FlagMaxGas)
	if maxGas < 0 {
		errors.New("max-gas flag not correct")
	}

	bech32add, _ := types.GetAddrFromBech32("address1k65clfmyr30v20lga42srah0x7er95ludlyme4")
	tx := transfer.TxTransfer{
		Senders: transfertypes.TransItems{
			{types.Address(bech32add), types.NewInt(1), nil},
		},
		Receivers: transfertypes.TransItems{
			{types.Address(bech32add), types.NewInt(1), nil},
		},
	}
	txStd := txs.NewTxStd(tx, "test", types.NewInt(maxGas))

	signers := getSigners(ctx, txStd.ITx.GetSigner())
	for _, signerName := range signers {
		txStd, _ = signStdTx(ctx, signerName, signerNonce[signerName] + txNumber, txStd, "")
	}

	bz := ctx.Codec.MustMarshalBinaryBare(txStd)
	_, err := ctx.BroadcastTxSync(bz)
	if err != nil {
		fmt.Println("BroadcastTx error status: ", err)
	}
	return bz, nil
}

func getSigners(ctx clictx.CLIContext, txSignerAddrs []types.Address) []string {
	var sortNames []string
	for _, addr := range txSignerAddrs {
		keybase, err := keys.GetKeyBaseFromDir(ctx, "/Users/shen/.qoscli")
		info, _ := keybase.Get("mock1")

		if err != nil {
			panic(err.Error())
		}

		info, err = keybase.GetByAddress(addr)
		if err != nil {
			panic(fmt.Sprintf("signer addr: %s not in current keybase. err:%s", addr, err.Error()))
		}

		sortNames = append(sortNames, info.GetName())
	}

	return sortNames
}

func getDefaultAccountNonce(ctx clictx.CLIContext, address []byte) (int64, error) {
	if ctx.NonceNodeURI == "" {
		return account.GetAccountNonce(ctx, address)
	}

	//NonceNodeURI不为空,使用NonceNodeURI查询account nonce值
	rpc := tmrpc.NewHTTP(ctx.NonceNodeURI, "/websocket")
	newCtx := clictx.NewCLIContext().WithClient(rpc).WithCodec(ctx.Codec)

	return account.GetAccountNonce(newCtx, address)
}

func signStdTx(ctx clictx.CLIContext, signerKeyName string, nonce int64, txStd *txs.TxStd, fromChainID string) (*txs.TxStd, error) {

	info, err := keys.GetKeyInfo(ctx, signerKeyName)
	if err != nil {
		return nil, err
	}

	addr := info.GetAddress()
	ok := false

	for _, signer := range txStd.GetSigners() {
		if bytes.Equal(addr.Bytes(), signer.Bytes()) {
			ok = true
		}
	}

	if !ok {
		return nil, fmt.Errorf("Name %s is not signer", signerKeyName)
	}

	sigdata := txStd.BuildSignatureBytes(nonce, fromChainID)
	sig, pubkey := signData(ctx, signerKeyName, sigdata)

	txStd.Signature = append(txStd.Signature, txs.Signature{
		Pubkey:    pubkey,
		Signature: sig,
		Nonce:     nonce,
	})

	return txStd, nil
}

func signData(ctx clictx.CLIContext, name string, data []byte) ([]byte, crypto.PubKey) {
	pass := "12345678"
	//pass, err := keys.GetPassphrase(ctx, name)
	////fmt.Println("pass is:", pass)
	//if err != nil {
	//	panic(fmt.Sprintf("Get %s Passphrase error: %s", name, err.Error()))
	//}

	keybase, err := keys.GetKeyBase(ctx)
	if err != nil {
		panic(err.Error())
	}

	sig, pubkey, err := keybase.Sign(name, pass, data)
	if err != nil {
		panic(err.Error())
	}

	return sig, pubkey
}

func (t *transacter) receiveLoop(connIndex int) {
	c := t.conns[connIndex]
	defer t.endingWg.Done()
	for {
		_, _, err := c.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				t.logger.Error(
					fmt.Sprintf("failed to read response on conn %d", connIndex),
					"err",
					err,
				)
			}
			return
		}
		if t.stopped || t.connsBroken[connIndex] {
			return
		}
	}
}

// Stop closes the connections.
func (t *transacter) Stop() {
	t.stopped = true
	t.endingWg.Wait()
	fmt.Println("conns stoped .....")
	for _, c := range t.conns {

		c.Close()
	}
}