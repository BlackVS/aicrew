package agent

import "math"

// A turn's own cost (docs/DESIGN-TURNS.md, increment 0). Claude Code's
// result reports total_cost_usd and modelUsage as running totals of the
// whole conversation, so a resumed turn's figure includes every earlier
// turn; only its usage is the turn's own. The cost is therefore derived:
// the turn's tokens at the list prices below when every model it used is
// priced here, otherwise the difference of the client's running total
// since the conversation's previous turn.

// The cost sources a turn record names.
const (
	// CostPriceList: the turn's own tokens at PriceList's prices.
	CostPriceList = "price_list"
	// CostClientTotal: the difference of the client's running total since
	// the conversation's previous turn.
	CostClientTotal = "client_total"
	// CostUnpriced: neither was available, so the turn's cost is unknown
	// and counted as 0; its tokens are still recorded.
	CostUnpriced = "unpriced"
)

// PriceListVersion names PriceList's prices, for the turn record.
const PriceListVersion = "2026-10-15"

// modelPrice is a model's list price in USD per million tokens. A zero
// price is unknown, and a turn that used tokens of that kind is not priced
// from the list.
type modelPrice struct {
	Input, Output, CacheRead, CacheWrite5m, CacheWrite1h float64
}

// PriceList holds only prices confirmed against the client's own figures:
// claude-opus-5-5 by the turn records of 2026-10-15, whose running-total
// differences these prices reproduce to the cent. The 5-minute cache-write
// price was not observed.
var PriceList = map[string]modelPrice{
	"claude-opus-5-5": {Input: 4, Output: 20, CacheRead: 0.20, CacheWrite1h: 8},
}

// listCost is u's cost at model's list price, and whether the list prices
// every kind of token u holds. A cache write is priced only when its split
// between the 5-minute and the 1-hour cache is known.
func listCost(model string, u TurnUsage) (float64, bool) {
	p, ok := PriceList[model]
	if !ok || u.CacheWrite != u.CacheWrite5m+u.CacheWrite1h {
		return 0, false
	}
	usd := 0.0
	for _, k := range []struct {
		n     int64
		price float64
	}{{u.Input, p.Input}, {u.Output, p.Output}, {u.CacheRead, p.CacheRead},
		{u.CacheWrite5m, p.CacheWrite5m}, {u.CacheWrite1h, p.CacheWrite1h}} {
		if k.n == 0 {
			continue
		}
		if k.price == 0 {
			return 0, false
		}
		usd += float64(k.n) * k.price / 1e6
	}
	return math.Round(usd*1e6) / 1e6, true
}
