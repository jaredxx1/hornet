package monitor

import (
	"container/ring"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	socketio "github.com/googollee/go-socket.io"

	"github.com/iotaledger/hive.go/syncutils"
	"github.com/iotaledger/iota.go/trinary"

	"github.com/gohornet/hornet/packages/model/milestone_index"
	"github.com/gohornet/hornet/packages/model/tangle"
)

const (
	TX_BUFFER_SIZE = 50000
)

var (
	txRingBuffer          *ring.Ring
	txPointerMap          map[trinary.Hash]*wsTransaction
	bundleTransactionsMap map[trinary.Hash]map[trinary.Hash]struct{}

	txRingBufferLock = syncutils.Mutex{}
	broadcastLock    = syncutils.Mutex{}
)

type (
	wsTransaction struct {
		Hash       string `json:"hash"`
		Address    string `json:"address"`
		Value      int64  `json:"value"`
		Tag        string `json:"tag"`
		Confirmed  bool   `json:"confirmed"`
		Reattached bool   `json:"reattached"`
		Bundle     string `json:"bundle"`
		ReceivedAt int64  `json:"receivedAt"`
		ConfTime   int64  `json:"ctime"`
		Milestone  string `json:"milestone"`
		//TrunkTransaction  string `json:"trunk"`
		//BranchTransaction string `json:"branch"`
	}

	getRecentTransactions struct {
		TXHistory []*wsTransaction `json:"txHistory"`
	}

	wsReattachment struct {
		Hash string `json:"hash"`
	}

	wsUpdate struct {
		Hash     string `json:"hash"`
		ConfTime int64  `json:"ctime"`
	}

	wsNewMile struct {
		Hash      string `json:"hash"`
		Milestone string `json:"milestone"`
		ConfTime  int64  `json:"ctime"`
	}
)

func initRingBuffer() {
	txRingBuffer = ring.New(TX_BUFFER_SIZE)
	txPointerMap = make(map[trinary.Hash]*wsTransaction)
	bundleTransactionsMap = make(map[trinary.Hash]map[trinary.Hash]struct{})
}

func onConnectHandler(s socketio.Conn) error {
	infoMsg := "Monitor client connection established"
	if s != nil {
		infoMsg = fmt.Sprintf("%s (ID: %v)", infoMsg, s.ID())
	}
	log.Info(infoMsg)
	socketioServer.JoinRoom("broadcast", s)
	return nil
}

func onErrorHandler(s socketio.Conn, e error) {
	errorMsg := "Monitor meet error"
	if e != nil {
		errorMsg = fmt.Sprintf("%s: %s", errorMsg, e.Error())
	}
	log.Error(errorMsg)
}

func onDisconnectHandler(s socketio.Conn, msg string) {
	infoMsg := "Monitor client connection closed"
	if s != nil {
		infoMsg = fmt.Sprintf("%s (ID: %v)", infoMsg, s.ID())
	}
	log.Info(fmt.Sprintf("%s: %s", infoMsg, msg))
	socketioServer.LeaveAllRooms(s)
}

func onNewTx(tx *tangle.CachedTransaction) {

	tx.RegisterConsumer() //+1
	iotaTx := tx.GetTransaction().Tx
	tx.Release() //-1

	wsTx := &wsTransaction{
		Hash:       iotaTx.Hash,
		Address:    iotaTx.Address,
		Value:      iotaTx.Value,
		Tag:        iotaTx.Tag,
		Bundle:     iotaTx.Bundle,
		ReceivedAt: time.Now().Unix() * 1000,
		ConfTime:   1111111111111,
		Milestone:  "f",
		//TrunkTransaction:  tx.Tx.TrunkTransaction,
		//BranchTransaction: tx.Tx.BranchTransaction,
	}

	txRingBufferLock.Lock()

	if _, exists := txPointerMap[iotaTx.Hash]; exists {
		// Tx already exists => ignore
		txRingBufferLock.Unlock()
		return
	}

	// Delete old element from map
	if txRingBuffer.Value != nil {
		oldTx := txRingBuffer.Value.(*wsTransaction)
		if _, has := bundleTransactionsMap[oldTx.Bundle]; has {
			delete(bundleTransactionsMap[oldTx.Bundle], oldTx.Hash)
			if len(bundleTransactionsMap[oldTx.Bundle]) == 0 {
				delete(bundleTransactionsMap, oldTx.Bundle)
			}
		}
		delete(txPointerMap, oldTx.Hash)
	}

	// Set new element in ringbuffer
	txRingBuffer.Value = wsTx
	txRingBuffer = txRingBuffer.Next()

	// Add new element to map
	txPointerMap[wsTx.Hash] = wsTx
	if _, has := bundleTransactionsMap[wsTx.Bundle]; !has {
		bundleTransactionsMap[wsTx.Bundle] = make(map[trinary.Hash]struct{})
	}
	bundleTransactionsMap[wsTx.Bundle][wsTx.Hash] = struct{}{}

	txRingBufferLock.Unlock()

	broadcastLock.Lock()
	socketioServer.BroadcastToRoom("broadcast", "newTX", wsTx)
	broadcastLock.Unlock()
}

func onConfirmedTx(tx *tangle.CachedTransaction, msIndex milestone_index.MilestoneIndex, confTime int64) {

	tx.RegisterConsumer() //+1
	iotaTx := tx.GetTransaction().Tx
	tx.Release() //-1

	if iotaTx.CurrentIndex == 0 {
		// Tail Tx => Check if this is a value Tx
		bundle := tangle.GetBundleOfTailTransaction(iotaTx.Bundle, iotaTx.Hash)
		if bundle != nil {
			ledgerChanges, _ := bundle.GetLedgerChanges()
			if len(ledgerChanges) > 0 {
				// Mark all different Txs in all bundles as reattachment
				reattachmentWorkerPool.TrySubmit(iotaTx.Bundle)
			}
		}
	}

	txRingBufferLock.Lock()
	if wsTx, exists := txPointerMap[iotaTx.Hash]; exists {
		wsTx.Confirmed = true
		wsTx.ConfTime = confTime * 1000
	}
	txRingBufferLock.Unlock()

	update := wsUpdate{
		Hash:     iotaTx.Hash,
		ConfTime: confTime * 1000,
	}

	broadcastLock.Lock()
	socketioServer.BroadcastToRoom("broadcast", "update", update)
	broadcastLock.Unlock()
}

func onNewMilestone(bundle *tangle.Bundle) {

	tailTx := bundle.GetTail() //+1
	confTime := tailTx.GetTransaction().GetTimestamp() * 1000
	tailTx.Release() //-1

	transactions := bundle.GetTransactions() //+1

	txRingBufferLock.Lock()
	for _, tx := range transactions {
		if wsTx, exists := txPointerMap[tx.GetTransaction().GetHash()]; exists {
			wsTx.Confirmed = true
			wsTx.Milestone = "t"
			wsTx.ConfTime = confTime
		}
	}
	txRingBufferLock.Unlock()

	broadcastLock.Lock()
	for _, tx := range transactions {
		update := wsNewMile{
			Hash:      tx.GetTransaction().GetHash(),
			Milestone: "t",
			ConfTime:  confTime,
		}

		socketioServer.BroadcastToRoom("broadcast", "updateMilestone", update)
	}
	broadcastLock.Unlock()

	transactions.Release() //-1
}

func onReattachment(bundleHash trinary.Hash) {

	var updates []wsReattachment

	txRingBufferLock.Lock()
	if _, has := bundleTransactionsMap[bundleHash]; has {
		for txHash := range bundleTransactionsMap[bundleHash] {
			if wsTx, exists := txPointerMap[txHash]; exists {
				wsTx.Reattached = true
				updates = append(updates, wsReattachment{
					Hash: txHash,
				})
			}
		}
	}
	txRingBufferLock.Unlock()

	broadcastLock.Lock()
	for _, update := range updates {
		socketioServer.BroadcastToRoom("broadcast", "updateReattach", update)
	}
	broadcastLock.Unlock()
}

func setupResponse(c *gin.Context) {
	c.Header("Access-Control-Allow-Origin", "*")
	c.Header("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
	c.Header("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization")
}

func handleAPI(c *gin.Context) {
	setupResponse(c)

	amount := 15000
	amountStr := c.Query("amount")
	if amountStr != "" {
		amountParsed, err := strconv.Atoi(amountStr)
		if err == nil {
			amount = amountParsed
		}
	}

	var txs []*wsTransaction

	txRingBufferLock.Lock()

	txPointer := txRingBuffer
	for txCount := 0; txCount < amount; txCount++ {
		txPointer = txPointer.Prev()

		if (txPointer == nil) || (txPointer == txRingBuffer) || (txPointer.Value == nil) {
			break
		}

		txs = append(txs, txPointer.Value.(*wsTransaction))
	}

	txRingBufferLock.Unlock()

	response := &getRecentTransactions{}
	response.TXHistory = txs

	c.JSON(http.StatusOK, response)
}
