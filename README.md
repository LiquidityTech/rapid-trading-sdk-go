# Rapid Trading SDK for golang

---

## Usage

go get github.com/LiquidityTech/rapid-trading-sdk-go

## Example

```go
package main

import (
	"context"
	"fmt"

	"github.com/LiquidityTech/rapid-trading-sdk-go"
)

func main() {
	c := rapid.NewClient("yourKey", "yourSeccet")
	pairs, err := c.GetPairs(context.TODO(), rapid.GetPairsReq{
		Name:     "WBNB-BUSD@PANCAKESWAP",
		Exchange: "",
	})
	fmt.Println(pairs, err)
}
```