package rapid

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shopspring/decimal"
)

const (
	pingPeriod           = 45 * time.Second
	pongWait             = 50 * time.Second
	writeWait            = 20 * time.Second
	maxMessageSize       = 409600
	clientSendChanBuffer = 20

	ChannelPrice Channel = "price" // 订阅交易对最新价格
	ChannelOrder Channel = "order" // 订单

	OpSubscribe   Operation = "subscribe"   // 订阅
	OpUnsubscribe Operation = "unsubscribe" // 取消订阅
	OpOrder       Operation = "order"       // 下单

	MsgTypeSubscribed   MsgType = "subscribed"
	MsgTypeUnsubscribed MsgType = "unsubscribed"
	MsgTypeOrder        MsgType = "ordered"

	MsgTypeUpdate MsgType = "update" // 数据更新
)

type Operation string
type Channel string
type MsgType string

type ReqMessage struct {
	Id      uint64      `json:"id"`      // 请求ID
	Op      Operation   `json:"op"`      // 操作
	Channel Channel     `json:"channel"` // 频道
	Args    interface{} `json:"args"`    // 参数
}

type RespMessage struct {
	Id      uint64          `json:"id,omitempty"`
	Type    MsgType         `json:"type"`    // 消息类型
	Channel Channel         `json:"channel"` // 频道
	Data    json.RawMessage `json:"data"`    // 数据
}

type ConfirmMessage struct {
	RespMessage
	Data ConfirmData `json:"data"`
}

func (confirm ConfirmMessage) IsSuccess() bool {
	return confirm.Data.Code == 0
}

type PairArgs []string

type OrderArgs struct {
	Pair              string          `json:"pair" validate:"required"`
	Type              string          `json:"type" validate:"required"`
	TokenSymbolIn     string          `json:"tokenSymbolIn" validate:"required"`
	AmountIn          decimal.Decimal `json:"amountIn" validate:"required"`
	AmountOutMin      decimal.Decimal `json:"amountOutMin" validate:"required"`
	GasPriceMax       decimal.Decimal `json:"gasPriceMax" validate:"required"`
	TargetBlockNumber uint64          `json:"targetBlockNumber" validate:"required"`
}

type ConfirmData struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type PriceData struct {
	Timestamp   int64  `json:"ts"` // 毫秒时间戳
	BlockNumber uint64 `json:"n"`  // 区块号
	BlockTime   uint64 `json:"bt"` // 区块头时间戳
	Pair        string `json:"p"`  // 交易对名称
	R0          string `json:"r0"`
	R1          string `json:"r1"`
}

type OrderResultData struct {
	Id            uint64          `json:"id"` // 任务id
	Pair          string          `json:"pair"`
	TokenSymbolIn string          `json:"tokenSymbolIn"`
	Success       bool            `json:"success"`
	BlockNumber   uint64          `json:"blockNumber"`
	AmountIn      decimal.Decimal `json:"amountIn"`
	AmountOut     decimal.Decimal `json:"amountOut"`
	GasFee        decimal.Decimal `json:"gasFee"`
	Hash          string          `json:"hash"`
}

type WsClient struct {
	count          uint64
	conn           *websocket.Conn
	send           chan *ReqMessage
	closed         chan bool
	closedOnce     sync.Once
	messageHandler MessageHandler
	callbacks      map[uint64]Callback
	callbackMutex  sync.Mutex
	Logger         Logger
}

func NewWsClient(conn *websocket.Conn, logger Logger) *WsClient {
	client := &WsClient{
		conn:           conn,
		send:           make(chan *ReqMessage, clientSendChanBuffer),
		closed:         make(chan bool),
		messageHandler: nil,
		callbacks:      make(map[uint64]Callback),
		Logger:         logger,
	}

	go client.writePump()
	go client.readPump()
	return client
}

// readPump pumps messages from the websocket connection to the hub.
//
// The application runs readPump in a per-connection goroutine. The application
// ensures that there is at most one reader on a connection by executing all
// reads from this goroutine.
func (c *WsClient) readPump() {
	defer func() {
		c.Close()
		c.Logger.Infof("stop readPump of client %v", c.conn.RemoteAddr())
	}()
	c.conn.SetReadLimit(maxMessageSize)
	err := c.conn.SetReadDeadline(time.Now().Add(pongWait))
	if err != nil {
		c.Logger.Errorf(err.Error())
	}
	c.conn.SetPongHandler(func(string) error {
		c.Logger.Infof("received pong message from peer")
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				c.Logger.Infof("websocket read message error: %v", err)
			}
			break
		}
		var respMessage RespMessage
		if err := json.Unmarshal(message, &respMessage); err != nil {
			c.Logger.Errorf("unmarshal reqMessage error: %v", err)
			continue
		}
		if respMessage.Id != 0 {
			c.callbackMutex.Lock()
			callback := c.callbacks[respMessage.Id]
			delete(c.callbacks, respMessage.Id)
			c.callbackMutex.Unlock()
			if callback != nil && respMessage.Type != MsgTypeUpdate {
				confirm := ConfirmData{}
				if err = json.Unmarshal(respMessage.Data, &confirm); err != nil {
					c.Logger.Errorf(err.Error())
					continue
				}
				callback(ConfirmMessage{
					RespMessage: respMessage,
					Data:        confirm,
				})
			}
		}
		handler := c.messageHandler
		if handler == nil {
			// can't handle message with this type
			continue
		}

		err = handler(c, &respMessage)
		if err != nil {
			c.Logger.Errorf("handle message error: %v", err)
			continue
		}
	}
}

func (c *WsClient) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.Close()
		c.Logger.Infof("stop writePump of client %v", c.conn.RemoteAddr())
	}()
	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				c.Logger.Errorf("get next writer error: %v", err)
				return
			}
			messageBytes, err := json.Marshal(message)
			if err != nil {
				c.Logger.Errorf(err.Error())
				continue
			}
			nn, err := w.Write(messageBytes)
			if err != nil || nn != len(messageBytes) {
				c.Logger.Errorf("writer of client write error: %v", err)
				return
			}

			if err := w.Close(); err != nil {
				c.Logger.Errorf("close writer of client error: %v", err)
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-c.closed:
			return
		}
	}
}

func (c *WsClient) Close() {
	c.conn.Close()
	c.setClose()
}

func (c *WsClient) setClose() {
	c.closedOnce.Do(func() {
		close(c.closed)
	})
}

func (c *WsClient) IsClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *WsClient) getId() uint64 {
	return atomic.AddUint64(&c.count, 1)
}

func (c *WsClient) Send(message *ReqMessage) {
	select {
	case c.send <- message:
	default:
		c.Logger.Errorf("send buffer overflow, can not delivery message")
	}
}

type MessageHandler func(c *WsClient, resp *RespMessage) (err error)

type Callback func(confirm ConfirmMessage)

func (c *WsClient) SendReq(op Operation, channel Channel, args interface{}, callback Callback) {
	id := c.getId()
	req := &ReqMessage{
		Id:      id,
		Op:      op,
		Channel: channel,
		Args:    args,
	}
	c.Send(req)

	if callback != nil {
		c.callbackMutex.Lock()
		c.callbacks[id] = callback
		c.callbackMutex.Unlock()
	}
}

func (c *WsClient) SendReqAndWait(op Operation, channel Channel, args interface{}) (confirm *ConfirmData, err error) {
	completed := make(chan struct{})
	timeout := time.After(5 * time.Second)
	result := ConfirmMessage{}
	callback := func(confirm ConfirmMessage) {
		result = confirm
		close(completed)
	}
	c.SendReq(op, channel, args, callback)
	select {
	case <-completed:
		if result.IsSuccess() {
			return &result.Data, nil
		} else {
			return confirm, fmt.Errorf("action failed: %v", result.Data.Msg)
		}
	case <-timeout:
		return confirm, errors.New("action failed: timeout")
	}
}
