package rapid

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shopspring/decimal"
	"github.com/umbracle/ethgo"
)

const (
	baseHTTPURL = "https://rapidtrading-api.liquiditytech.com"
	baseWsURL   = "wss://rapidtrading-api.liquiditytech.com"
	httpTimeout = 15 * time.Second
)

type Logger interface {
	Infof(msg string, args ...interface{})
	Errorf(msg string, args ...interface{})
}

type CommonResp struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type Client struct {
	APIKey      string
	SecretKey   string
	BaseHTTPURL string
	BaseWsURL   string
	HTTPClient  *http.Client
	Debug       bool
	Logger      Logger

	simplexClient *WsClient // 不涉及推送流时使用
	simplexMutex  sync.Mutex
}

func NewClient(apiKey, secretKey string) *Client {
	return &Client{
		APIKey:      apiKey,
		SecretKey:   secretKey,
		BaseHTTPURL: baseHTTPURL,
		BaseWsURL:   baseWsURL,
		HTTPClient: &http.Client{
			Timeout: httpTimeout,
		},
		Logger: logImp{log.New(os.Stderr, "[FlashNet] ", log.LstdFlags)},
	}
}

type logImp struct {
	stdLog *log.Logger
}

func (l logImp) Infof(msg string, data ...interface{}) {
	l.stdLog.Printf(msg, data...)
}

func (l logImp) Errorf(msg string, data ...interface{}) {
	l.stdLog.Printf(msg, data...)
}

func (c *Client) signParams(params url.Values) (sign string) {
	if params == nil {
		params = make(url.Values)
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sb := strings.Builder{}
	for _, k := range keys {
		value := params.Get(k)
		if value == "" {
			continue
		}
		sb.WriteString(k + "=" + value + "&")
	}
	payload := strings.TrimSuffix(sb.String(), "&")
	h := hmac.New(sha256.New, []byte(c.SecretKey))
	h.Write([]byte(payload))
	sign = hex.EncodeToString(h.Sum(nil))
	params.Del("apiKeyParamName")
	return sign
}

func getTimestamp() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05")
}

func (c *Client) callAPI(ctx context.Context, method string, path string, query url.Values, body interface{}, target interface{}) (statusCode int, respBody []byte, err error) {
	u, err := url.Parse(fmt.Sprintf("%v%v", c.BaseHTTPURL, path))
	if err != nil {
		return
	}
	query, bodyBytes, err := c.renderSign(query, body)
	if err != nil {
		return statusCode, respBody, err
	}
	u.RawQuery = query.Encode()
	urlStr := u.String()
	req, err := http.NewRequestWithContext(ctx, method, urlStr, bytes.NewReader(bodyBytes))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		c.Logger.Errorf("api get error, url: %v, err: %v", urlStr, err)
		return
	}
	defer resp.Body.Close()

	respBody, err = io.ReadAll(resp.Body)
	statusCode = resp.StatusCode
	if statusCode != http.StatusOK {
		err = fmt.Errorf("api get abnormal response, url: %v, code: %v, response: %v", urlStr, resp.StatusCode, string(respBody))
		return
	}
	common := CommonResp{}
	if err = json.Unmarshal(respBody, &common); err != nil {
		return
	}
	if common.Code != 0 {
		err = fmt.Errorf("api data code: %v, msg: %v", common.Code, common.Msg)
		return
	}
	if target != nil {
		err = json.Unmarshal(common.Data, target)
	}
	return resp.StatusCode, respBody, err
}

func (c *Client) renderSign(query url.Values, body interface{}) (newQuery url.Values, bodyBytes []byte, err error) {
	if query == nil {
		query = url.Values{}
	}
	query.Add("apiKey", c.APIKey)
	query.Add("timestamp", getTimestamp())

	var (
		bodyParams = url.Values{}
		signParams = url.Values{}
	)
	for k, v := range query {
		signParams[k] = v
	}
	if body != nil {
		m, err := StructToMap(body, "json")
		if err != nil {
			return nil, nil, err
		}
		for k, v := range m {
			bodyParams.Set(k, fmt.Sprintf("%v", v))
		}
		bodyBytes, err = json.Marshal(m)
		if err != nil {
			return nil, nil, err
		}
	}
	for k, v := range bodyParams {
		signParams[k] = v
	}
	sign := c.signParams(signParams)
	query.Add("sign", sign)
	return query, bodyBytes, nil
}

func (c *Client) Close() error {
	c.simplexClient.Close()
	return nil
}

func (c *Client) GetSimplexStream() (wsClient *WsClient, err error) {
	if c.simplexClient != nil && !c.simplexClient.IsClosed() {
		return c.simplexClient, nil
	}
	c.simplexMutex.Lock()
	defer c.simplexMutex.Unlock()

	if c.simplexClient != nil && !c.simplexClient.IsClosed() {
		return c.simplexClient, nil
	}
	wsClient, err = c.NewStream()
	if err != nil {
		return nil, err
	}
	c.simplexClient = wsClient
	return wsClient, nil
}

func (c *Client) NewStream() (wsClient *WsClient, err error) {
	u, err := url.Parse(c.BaseWsURL + "/ws")
	if err != nil {
		return nil, err
	}
	query, _, err := c.renderSign(nil, nil)
	if err != nil {
		return nil, err
	}
	u.RawQuery = query.Encode()
	wsConn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return nil, err
	}
	wsClient = NewWsClient(wsConn, c.Logger)
	return wsClient, nil
}

func (c *Client) SubscribePrice(pairs []string, ch chan *PriceData) (cancel func(), err error) {
	ws, err := c.NewStream()
	if err != nil {
		return nil, err
	}
	cancel = func() {
		ws.Close()
	}
	defer func() {
		if err != nil {
			cancel()
		}
	}()
	if _, err = ws.SendReqAndWait(OpSubscribe, ChannelPrice, pairs); err != nil {
		return nil, err
	}
	ws.messageHandler = genSubscribeHandler(ChannelPrice, ch)
	return cancel, nil
}

func (c *Client) SubscribeOrderResult(ch chan *OrderResultData) (cancel func(), err error) {
	ws, err := c.NewStream()
	if err != nil {
		return nil, err
	}
	cancel = func() {
		ws.Close()
	}
	defer func() {
		if err != nil {
			cancel()
		}
	}()
	if _, err = ws.SendReqAndWait(OpSubscribe, ChannelOrder, nil); err != nil {
		return nil, err
	}
	ws.messageHandler = genSubscribeHandler(ChannelOrder, ch)
	return cancel, nil
}

func genSubscribeHandler[T any](channel Channel, ch chan *T) MessageHandler {
	return func(c *WsClient, resp *RespMessage) (err error) {
		if resp.Type == MsgTypeUpdate && resp.Channel == channel {
			data := new(T)
			if err = json.Unmarshal(resp.Data, data); err != nil {
				return err
			}
			select {
			case ch <- data:
			default:
				c.Logger.Errorf("channel is overflowed, ignore new data")
			}
		}
		return nil
	}
}

type CreateOrderReq struct {
	Pair              string `json:"pair"`
	Type              string `json:"type"`
	TokenSymbolIn     string `json:"tokenSymbolIn"`
	AmountIn          string `json:"amountIn"`
	AmountOutMin      string `json:"amountOutMin"`
	GasPriceMax       string `json:"gasPriceMax"`
	TargetBlockNumber uint64 `json:"targetBlockNumber"`
}

type CreateOrderResp struct {
	Id uint64 `json:"id"`
}

func (c *Client) CreateOrder(ctx context.Context, req CreateOrderReq) (resp *CreateOrderResp, err error) {
	resp = &CreateOrderResp{}
	_, _, err = c.callAPI(ctx, http.MethodPost, "/api/v1/orders", nil, req, resp)
	return resp, err
}

func (c *Client) CreateOrderByStream(req CreateOrderReq) (resp *CreateOrderResp, err error) {
	ws, err := c.GetSimplexStream()
	if err != nil {
		return nil, err
	}
	confirm, err := ws.SendReqAndWait(OpOrder, ChannelOrder, req)
	if err != nil {
		return nil, err
	}
	resp = &CreateOrderResp{}
	err = json.Unmarshal(confirm.Data, resp)
	return resp, err
}

type (
	Network  string
	Exchange string
)

const (
	NetworkBSC Network = "BSC"
	NetworkETH Network = "ETH"

	ExchangePancakeSwap Exchange = "PANCAKESWAP"
	ExchangeBiswap      Exchange = "BISWAP"
)

type GetPairsReq struct {
	Name     string
	Exchange Exchange
}

type Pair struct {
	Id         uint64          `json:"id"`
	Name       string          `json:"name"` // BTC-USDT@BISWAP
	Addr       ethgo.Address   `json:"addr"`
	Network    Network         `json:"network"`
	Exchange   Exchange        `json:"exchange"`
	Fee        decimal.Decimal `json:"fee"`
	Token0Addr ethgo.Address   `json:"token0Addr"`
	Token1Addr ethgo.Address   `json:"token1Addr"`
	Token0     Token           `json:"token0"`
	Token1     Token           `json:"token1"`
}

type Token struct {
	Id       uint64        `json:"id"`
	Addr     ethgo.Address `json:"addr"`
	Name     string        `json:"name"`
	Symbol   string        `json:"symbol"`
	Decimals uint8         `json:"decimals"`
}

func (c *Client) GetPairs(ctx context.Context, req GetPairsReq) (pairs []*Pair, err error) {
	pairs = make([]*Pair, 0)
	q := url.Values{}
	if req.Name != "" {
		q.Set("name", req.Name)
	}
	if req.Exchange != "" {
		q.Set("exchange", string(req.Exchange))
	}
	_, _, err = c.callAPI(ctx, http.MethodGet, "/api/v1/pairs", q, nil, &pairs)
	return pairs, err
}

type OrderResult struct {
	Id            uint64          `json:"id"` // 任务id
	Pair          string          `json:"pair"`
	TokenSymbolIn string          `json:"tokenSymbolIn"`
	Success       bool            `json:"success"`
	AmountIn      decimal.Decimal `json:"amountIn"`
	AmountOut     decimal.Decimal `json:"amountOut"`
	GasFee        decimal.Decimal `json:"gasFee"`
	Hash          string          `json:"hash"`
}

func (c *Client) GetOrderResult(ctx context.Context, orderId uint64) (result *OrderResult, err error) {
	path := fmt.Sprintf("/api/v1/orders/%s/result", strconv.FormatUint(orderId, 10))
	result = new(OrderResult)
	_, _, err = c.callAPI(ctx, http.MethodGet, path, nil, nil, result)
	if err != nil {
		return nil, err
	}
	return result, nil
}

type GetTokenBalancesReq struct {
	Account string
	Tokens  []string
}

type TokenBalances map[ethgo.Address]decimal.Decimal

func (c *Client) GetTokenBalances(ctx context.Context, req GetTokenBalancesReq) (balances TokenBalances, err error) {
	balances = make(TokenBalances)
	q := url.Values{}
	q.Set("account", req.Account)
	q.Set("tokens", strings.Join(req.Tokens, ","))
	_, _, err = c.callAPI(ctx, http.MethodGet, "/api/v1/chain/token-balances", q, nil, &balances)
	return balances, err
}

// StructToMap convert struct to map[string]interface{}
func StructToMap(in interface{}, tagName string) (m map[string]interface{}, err error) {
	m = make(map[string]interface{})

	v := reflect.ValueOf(in)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}

	if v.Kind() != reflect.Struct {
		return nil, fmt.Errorf("StructToMap only accepts struct or struct pointer; got %T", v)
	}

	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		fi := t.Field(i)
		if tagValue := fi.Tag.Get(tagName); tagValue != "" {
			m[tagValue] = v.Field(i).Interface()
		}
	}
	return m, nil
}
