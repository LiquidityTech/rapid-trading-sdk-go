package rapid

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

var (
	ctx       = context.TODO()
	apiKey    = "apiKey"
	apiSecret = "apiSecret"
	pairs     = []string{"WBNB-BUSD@PANCAKESWAP", "WBNB-USDT@PANCAKESWAP"}
	c         = NewClient(apiKey, apiSecret)
)

func TestMain(m *testing.M) {
	setup()
	code := m.Run()
	teardown()
	os.Exit(code)
}

func setup() {

}

func teardown() {
}

func TestNewClient(t *testing.T) {
	ws, err := c.NewStream()
	assert.NoError(t, err)
	assert.False(t, ws.IsClosed())
}

func TestClient_SubscribePrice(t *testing.T) {
	ch := make(chan *PriceData, 20)
	cancel, err := c.SubscribePrice(pairs, ch)
	defer cancel()
	assert.NoError(t, err)
	count := 0
	notify := make(chan struct{})
	for price := range ch {
		found := false
		t.Logf("%#v", *price)
		for _, pair := range pairs {
			if price.Pair == pair {
				found = true
				count++
				break
			}
		}
		assert.True(t, found)
		if count >= 4 {
			close(notify)
			break
		}
	}
	select {
	case <-notify:
	case <-time.After(20 * time.Second):
		assert.Errorf(t, assert.AnError, "timeout")
	}
}

func TestClient_SubscribeOrderResult(t *testing.T) {
	ch := make(chan *OrderResultData, 20)
	cancel, err := c.SubscribeOrderResult(ch)
	defer cancel()
	assert.NoError(t, err)
}

func TestClient_CreateOrder(t *testing.T) {
	req := CreateOrderReq{
		Pair:              "UNI-BUSD@MDEX",
		Type:              "pga",
		TokenSymbolIn:     "BUSD",
		AmountIn:          "29750000000000000000",
		AmountOutMin:      "0",
		GasPriceMax:       "100000000000",
		TargetBlockNumber: 22366972,
	}
	resp, err := c.CreateOrder(ctx, req)
	assert.NoError(t, err)
	if assert.NotNil(t, resp) {
		assert.Greater(t, resp.Id, uint64(0))
		t.Logf("resp.Id %v", resp.Id)
	}
}

func TestClient_GetPairs(t *testing.T) {
	req := GetPairsReq{
		Name:     "WBNB-BUSD@PANCAKESWAP",
		Exchange: ExchangePancakeSwap,
	}
	pairs, err := c.GetPairs(ctx, req)
	assert.NoError(t, err)
	assert.Len(t, pairs, 1)
}

func TestClient_CreateOrderByStream(t *testing.T) {
	req := CreateOrderReq{
		Pair:              "WBNB-BUSD@PANCAKESWAP",
		Type:              "pga",
		TokenSymbolIn:     "WBNB",
		AmountIn:          "0.1",
		AmountOutMin:      "10",
		GasPriceMax:       "100",
		TargetBlockNumber: 21675044,
	}
	resp, err := c.CreateOrderByStream(req)
	assert.NoError(t, err)
	if assert.NotNil(t, resp) {
		assert.Greater(t, resp.Id, uint64(0))
		t.Logf("resp.Id %v", resp.Id)
	}
}

func TestClient_GetOrderResult(t *testing.T) {
	result, err := c.GetOrderResult(ctx, 7042433021000)
	assert.NoError(t, err)
	t.Log(result)
}

func TestClient_GetTokenBalances(t *testing.T) {
	req := GetTokenBalancesReq{
		Account: "0xbe807dddb074639cd9fa61b47676c064fc50d62c",
		Tokens:  []string{"0x7130d2A12B9BCbFAe4f2634d864A1Ee1Ce3Ead9c", "0xe9e7CEA3DedcA5984780Bafc599bD69ADd087D56", "0x55d398326f99059fF775485246999027B3197955"},
	}
	balances, err := c.GetTokenBalances(ctx, req)
	assert.NoError(t, err)
	assert.Len(t, balances, len(req.Tokens))
	t.Log(balances)
}
