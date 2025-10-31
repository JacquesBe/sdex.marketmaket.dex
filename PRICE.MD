# Market Making Pricing Formula Documentation

This document provides a complete breakdown of the pricing formulas used in the SDEX market maker bot. All formulas are presented in their full, unsimplified form with detailed explanations of where each term originates.

---

## Overview

The market maker calculates two quotes:
- **`pBid`**: The price at which we are willing to buy the base asset
- **`pAsk`**: The price at which we are willing to sell the base asset

These quotes are dynamically adjusted based on:
1. External market conditions (CEX prices)
2. Order book microstructure
3. Inventory risk
4. Market volatility
5. Order book imbalances

---

## Core Pricing Formulas

### 1. Fair Price Calculation

```
fairPrice = (1 - blendWeight) * externalMidPrice + blendWeight * microPrice
```

**Components:**

- **`blendWeight`**: Configuration parameter (0 to 1)
  - **Source**: `s.BotConfig.StrategyOptions.BlendWeight`
  - **Location**: `strategy/strategy.go:65`
  - **Database field**: `strategyOptions.blendWeight` from MongoDB config
  - **Purpose**: Controls how much weight to give to the microprice vs external mid price

- **`externalMidPrice`**: The mid-market price from the external CEX (Binance)
  - **Calculation**: `(bestBid + bestAsk) / 2.0`
  - **Location**: `pricefeed/binance_feed.go:273`
  - **Source**: Derived from Binance WebSocket order book stream
    - `bestBid`: Best (highest) bid price from Binance order book
    - `bestAsk`: Best (lowest) ask price from Binance order book
  - **Raw data source**: Binance WebSocket `wss://stream.binance.com:9443/ws/{symbol}@depth{levels}@100ms`

- **`microPrice`**: The microprice calculated from the order book
  - **Calculation**: `(bestAsk * bestBidQty + bestBid * bestAskQty) / (bestBidQty + bestAskQty)`
  - **Location**: `pricefeed/binance_feed.go:276-282`
  - **Components**:
    - `bestBid`: Best bid price from Binance
    - `bestAsk`: Best ask price from Binance
    - `bestBidQty`: Size (quantity) at the best bid level
    - `bestAskQty`: Size (quantity) at the best ask level
  - **Purpose**: Weighted average price that accounts for size at top of book
  - **Fallback**: If `bestBidQty + bestAskQty = 0`, then `microPrice = externalMidPrice`

**Example:**
```
If blendWeight = 0.3
   externalMidPrice = 0.123456
   microPrice = 0.123460

Then:
fairPrice = (1 - 0.3) * 0.123456 + 0.3 * 0.123460
          = 0.7 * 0.123456 + 0.3 * 0.123460
          = 0.0864192 + 0.0370380
          = 0.1234572
```

---

### 2. Inventory Value Calculation

```
inventoryValue = qBase * fairPrice - qQuote
```

**Components:**

- **`qBase`**: Quantity of base asset held in account
  - **Source**: Horizon API balance query
  - **Location**: `strategy/strategy.go:55-56`
  - **Process**:
    1. Balance Monitor fetches account balances from Horizon API
    2. URL: `{horizonBaseURI}/accounts/{publicKey}`
    3. Parsed from `HorizonBalance.Balance` field (string)
    4. Converted to float64 using `fmt.Sscanf(baseBalance.Balance, "%f", &qBase)`
  - **Fetching logic**: `balance/monitor.go:107-133`
  - **Update frequency**: Controlled by `BalanceMonitorOptions.TickRate` (milliseconds)

- **`qQuote`**: Quantity of counter (quote) asset held in account
  - **Source**: Horizon API balance query (same as qBase)
  - **Location**: `strategy/strategy.go:58-60`
  - **Process**: Same as qBase but for counter asset

- **`fairPrice`**: As calculated in section 1 above

**Purpose:** Represents the net position value in terms of the quote asset. Positive values indicate we are "long" (holding excess base asset), negative values indicate we are "short" (holding excess quote asset).

---

### 3. Inventory Bias Value Calculation

```
inventoryBiasValue = 0.5 * (riskAversion * rollingVolatility * rollingVolatility) * inventoryValue
```

**Components:**

- **`0.5`**: Constant coefficient from inventory risk theory
  - **Purpose**: Mathematical constant derived from optimal market making theory

- **`riskAversion`**: Risk aversion parameter (γ - gamma)
  - **Source**: `s.BotConfig.StrategyOptions.RiskAversion`
  - **Location**: `strategy/strategy.go:66`
  - **Database field**: `strategyOptions.riskAversion` from MongoDB config
  - **Purpose**: Controls how aggressively the bot manages inventory risk

- **`rollingVolatility`**: Standard deviation of external mid prices over a time window
  - **Source**: `*features.RollingVolatility`
  - **Location**: `strategy/strategy.go:47`
  - **Calculation location**: `pricefeed/binance_feed.go:296-313`
  - **Calculation process**:
    1. Get historical window of features: `featuresBuffer.WindowSince(volWindowMs)`
    2. For each feature row in window, extract `ExternalMidPrice`
    3. Calculate sum: `sum = Σ(externalMidPrice[i])`
    4. Calculate sum of squares: `sumSq = Σ(externalMidPrice[i]²)`
    5. Calculate count: `n = len(window)`
    6. Calculate mean: `mean = sum / n`
    7. Calculate variance: `variance = (sumSq / n) - (mean * mean)`
    8. Calculate standard deviation: `stdDev = sqrt(variance)`
  - **Window size**: Controlled by `StrategyOptions.VolWindowMs` (milliseconds)
  - **Database field**: `strategyOptions.volWindowMs`
  - **Minimum data requirement**: At least 2 feature rows in window

- **`inventoryValue`**: As calculated in section 2 above

**Purpose:** This term shifts both bid and ask prices to incentivize rebalancing inventory toward zero. When long (positive inventoryValue), it lowers both prices to encourage selling. When short (negative inventoryValue), it raises both prices to encourage buying.

---

### 4. Half Spread Floor Conversion

```
halfSpreadFloor = (halfSpreadFloorBps / 10000.0) * externalMidPrice
```

**Components:**

- **`halfSpreadFloorBps`**: Configuration value in basis points
  - **Source**: `s.BotConfig.StrategyOptions.HalfSpreadFloor`
  - **Location**: `strategy/strategy.go:67`
  - **Database field**: `strategyOptions.halfSpreadFloor`
  - **Type**: float64 (basis points)

- **`externalMidPrice`**: As calculated in section 1
  - **Value**: `(bestBid + bestAsk) / 2.0` from Binance

- **`10000.0`**: Conversion factor from basis points to decimal fraction

**Purpose:** Converts the basis points configuration value into an absolute price value that scales with the external mid price. This means the half spread floor adjusts proportionally to the price level.

**Example:**
```
If halfSpreadFloorBps = 5.0 bps
   externalMidPrice = 0.123456

Then:
halfSpreadFloor = (5.0 / 10000.0) * 0.123456
                = 0.0005 * 0.123456
                = 0.0000617280
```

**Location**: `strategy/strategy.go:86`

---

### 5. Half Spread Bid Calculation

```
halfSpreadBid = halfSpreadFloor + volatilitySensitivity * rollingVolatility + inventoryBias * max(0, inventoryValue) + obImbalanceSensitivity * bidPenalty
```

**Components:**

- **`halfSpreadFloor`**: As calculated in section 4

- **`volatilitySensitivity`**: Multiplier for volatility-based spread widening
  - **Source**: `s.BotConfig.StrategyOptions.VolatilitySensitivity`
  - **Location**: `strategy/strategy.go:68`
  - **Database field**: `strategyOptions.volatilitySensitivity`
  - **Purpose**: Controls how much to widen spread in response to market volatility

- **`rollingVolatility`**: As calculated in section 3 above

- **`inventoryBias`**: Coefficient for inventory-based spread asymmetry
  - **Source**: `s.BotConfig.StrategyOptions.InventoryBias`
  - **Location**: `strategy/strategy.go:69`
  - **Database field**: `strategyOptions.inventoryBias`
  - **Purpose**: Widens the bid side when holding excess base inventory

- **`max(0, inventoryValue)`**: Maximum of 0 and inventoryValue
  - **Purpose**: Only applies penalty when we have positive inventory (long position)
  - **Formula**: If `inventoryValue > 0` then use `inventoryValue`, else use `0`

- **`obImbalanceSensitivity`**: Order book imbalance sensitivity parameter
  - **Source**: `s.BotConfig.StrategyOptions.OBImbalanceSensitivity`
  - **Location**: `strategy/strategy.go:70`
  - **Database field**: `strategyOptions.obImbalanceSensitivity`
  - **Purpose**: Controls how much to adjust spread based on order book imbalance

- **`bidPenalty`**: Penalty term for bid side based on order book imbalance
  - **Source**: `features.BidPenalty`
  - **Location**: `strategy/strategy.go:93`
  - **Calculation**: `max(0, -tob)`
  - **Calculation location**: `pricefeed/binance_feed.go:291`
  - **Where `tob` is calculated**:
    ```
    tob = (bestBidQty - bestAskQty) / (bestBidQty + bestAskQty)
    ```
  - **Location of tob**: `pricefeed/binance_feed.go:285-288`
  - **Components**:
    - `bestBidQty`: Size at best bid from Binance order book
    - `bestAskQty`: Size at best ask from Binance order book
  - **Purpose**: Ranges from -1 (ask-heavy) to +1 (bid-heavy)
  - **bidPenalty interpretation**: When TOB is negative (more size on ask side), bidPenalty is positive, widening our bid spread

**Location**: `strategy/strategy.go:90-93`

**Purpose:** Determines the distance between fair price and bid quote, accounting for base spread, volatility, inventory position, and order book dynamics.

---

### 6. Half Spread Ask Calculation

```
halfSpreadAsk = halfSpreadFloor + volatilitySensitivity * rollingVolatility + inventoryBias * max(0, -inventoryValue) + obImbalanceSensitivity * askPenalty
```

**Components:**

- **`halfSpreadFloor`**: As calculated in section 4

- **`volatilitySensitivity`**: As described in section 5

- **`rollingVolatility`**: As described in section 3

- **`inventoryBias`**: As described in section 5

- **`max(0, -inventoryValue)`**: Maximum of 0 and negative inventoryValue
  - **Purpose**: Only applies penalty when we have negative inventory (short position)
  - **Formula**: If `inventoryValue < 0` then use `-inventoryValue`, else use `0`
  - **Note**: This is the opposite of the bid calculation

- **`obImbalanceSensitivity`**: As described in section 5

- **`askPenalty`**: Penalty term for ask side based on order book imbalance
  - **Source**: `features.AskPenalty`
  - **Location**: `strategy/strategy.go:100`
  - **Calculation**: `max(0, tob)`
  - **Calculation location**: `pricefeed/binance_feed.go:292`
  - **Where `tob` is calculated**: Same as in section 5
  - **askPenalty interpretation**: When TOB is positive (more size on bid side), askPenalty is positive, widening our ask spread

**Location**: `strategy/strategy.go:97-100`

**Purpose:** Determines the distance between fair price and ask quote, with inventory bias applied to the opposite side compared to bid.

---

### 7. Final Bid Price Calculation

```
pBid = fairPrice - inventoryBiasValue - halfSpreadBid
```

**Components:**

- **`fairPrice`**: As calculated in section 1
- **`inventoryBiasValue`**: As calculated in section 3
- **`halfSpreadBid`**: As calculated in section 5

**Location**: `strategy/strategy.go:103`

**Breakdown:**
1. Start with fair price
2. Subtract inventory bias (shifts price down when long, up when short)
3. Subtract bid half-spread (moves bid below fair price)

**Example:**
```
fairPrice = 0.123457
inventoryBiasValue = 0.000010 (we are long, so this pushes price down)
halfSpreadBid = 0.000050

pBid = 0.123457 - 0.000010 - 0.000050
     = 0.123397
```

---

### 8. Final Ask Price Calculation

```
pAsk = fairPrice - inventoryBiasValue + halfSpreadAsk
```

**Components:**

- **`fairPrice`**: As calculated in section 1
- **`inventoryBiasValue`**: As calculated in section 3
- **`halfSpreadAsk`**: As calculated in section 6

**Location**: `strategy/strategy.go:104`

**Breakdown:**
1. Start with fair price
2. Subtract inventory bias (same shift as bid side)
3. Add ask half-spread (moves ask above fair price)

**Example:**
```
fairPrice = 0.123457
inventoryBiasValue = 0.000010 (we are long, so this pushes price down)
halfSpreadAsk = 0.000055

pAsk = 0.123457 - 0.000010 + 0.000055
     = 0.123502
```

---

## Data Flow Summary

### External Data Sources

1. **Binance WebSocket** → Order book data (bids, asks, quantities)
   - Endpoint: `wss://stream.binance.com:9443/ws/{symbol}@depth{levels}@100ms`
   - Update frequency: ~100ms
   - Data: Best bid/ask prices and quantities, full order book depth

2. **Horizon API** → Stellar account balances (base and counter assets)
   - Endpoint: `{horizonBaseURI}/accounts/{publicKey}`
   - Update frequency: Configured via `BalanceMonitorOptions.TickRate`
   - Data: Asset balances for the market maker account

3. **MongoDB** → Bot configuration parameters
   - Collection: `reference.data.marketMakerConfig`
   - Loaded once at startup via CONFIG_ID environment variable
   - Data: All strategy parameters (blendWeight, riskAversion, etc.)

### Processing Pipeline

```
Binance WebSocket
    ↓
OrderBook Update (binance_feed.go:542)
    ↓
Compute Best Bid/Ask & Quantities (binance_feed.go:563)
    ↓
Calculate External Mid Price (binance_feed.go:273)
    ↓
Calculate Microprice (binance_feed.go:276-282)
    ↓
Calculate TOB (binance_feed.go:285-288)
    ↓
Calculate Bid/Ask Penalties (binance_feed.go:291-292)
    ↓
Calculate Rolling Volatility (binance_feed.go:296-313)
    ↓
Build FeaturesRow (binance_feed.go:315-322)
    ↓
Strategy Engine: Compute Fair Price (strategy.go:73)
    ↓
Strategy Engine: Compute Inventory Value (strategy.go:80)
    ↓
Strategy Engine: Compute Inventory Bias (strategy.go:83)
    ↓
Strategy Engine: Convert Half Spread Floor (strategy.go:86)
    ↓
Strategy Engine: Compute Half Spreads (strategy.go:90-100)
    ↓
Strategy Engine: Compute Final Quotes (strategy.go:103-104)
    ↓
Output: pBid, pAsk
```

---

## Configuration Parameters Reference

All strategy parameters are stored in MongoDB and loaded at startup:

| Parameter | MongoDB Field | Type | Location in Code | Purpose |
|-----------|---------------|------|------------------|---------|
| `blendWeight` | `strategyOptions.blendWeight` | float64 | `strategy.go:65` | Weight for microprice vs external mid |
| `riskAversion` | `strategyOptions.riskAversion` | float64 | `strategy.go:66` | Inventory risk aversion (γ) |
| `halfSpreadFloorBps` | `strategyOptions.halfSpreadFloor` | float64 | `strategy.go:67` | Minimum half-spread in bps |
| `volatilitySensitivity` | `strategyOptions.volatilitySensitivity` | float64 | `strategy.go:68` | Volatility spread multiplier |
| `inventoryBias` | `strategyOptions.inventoryBias` | float64 | `strategy.go:69` | Inventory asymmetry coefficient |
| `obImbalanceSensitivity` | `strategyOptions.obImbalanceSensitivity` | float64 | `strategy.go:70` | Order book imbalance sensitivity |
| `volWindowMs` | `strategyOptions.volWindowMs` | int64 | `binance_feed.go:438` | Rolling volatility window (ms) |

---

## Validation Checks

Before returning quotes, the following sanity checks are performed (`strategy/strategy.go:107-115`):

1. **Positive Price Check**:
   ```
   if pBid <= 0 || pAsk <= 0:
       return invalid quotes
   ```

2. **Crossed Quotes Check**:
   ```
   if pBid >= pAsk:
       return invalid quotes
   ```

These ensure the quotes are economically valid before use.

---

## Complete Formula Expansion

Combining all sections, here is the complete unsimplified formula:

### Bid Price:
```
pBid = [(1 - blendWeight) * ((bestBid + bestAsk) / 2) + blendWeight * ((bestAsk * bestBidQty + bestBid * bestAskQty) / (bestBidQty + bestAskQty))]
       - [0.5 * riskAversion * rollingVolatility² * (qBase * fairPrice - qQuote)]
       - [(halfSpreadFloorBps / 10000) * externalMidPrice + volatilitySensitivity * rollingVolatility + inventoryBias * max(0, qBase * fairPrice - qQuote) + obImbalanceSensitivity * max(0, -(bestBidQty - bestAskQty) / (bestBidQty + bestAskQty))]
```

### Ask Price:
```
pAsk = [(1 - blendWeight) * ((bestBid + bestAsk) / 2) + blendWeight * ((bestAsk * bestBidQty + bestBid * bestAskQty) / (bestBidQty + bestAskQty))]
       - [0.5 * riskAversion * rollingVolatility² * (qBase * fairPrice - qQuote)]
       + [(halfSpreadFloorBps / 10000) * externalMidPrice + volatilitySensitivity * rollingVolatility + inventoryBias * max(0, -(qBase * fairPrice - qQuote)) + obImbalanceSensitivity * max(0, (bestBidQty - bestAskQty) / (bestBidQty + bestAskQty))]
```

Where:
- `bestBid`, `bestAsk`, `bestBidQty`, `bestAskQty` come from Binance WebSocket
- `externalMidPrice` is `(bestBid + bestAsk) / 2` from Binance
- `qBase`, `qQuote` come from Horizon API
- `blendWeight`, `riskAversion`, `halfSpreadFloorBps`, `volatilitySensitivity`, `inventoryBias`, `obImbalanceSensitivity` come from MongoDB config
- `rollingVolatility` is calculated from historical `externalMidPrice` over `volWindowMs` window
- `fairPrice` is the first bracketed expression (used in inventory calculations)

---

## Notes

- All prices are in decimal format (not basis points) in calculations
- The `halfSpreadFloorBps` is converted from bps to an absolute price by: `(halfSpreadFloorBps / 10000) * externalMidPrice`
- This means the half spread floor scales with the price level (e.g., 5 bps of a 0.10 price = 0.00005, but 5 bps of a 1.00 price = 0.0005)
- Rolling volatility requires at least 2 data points in the window to be calculated
- Feature rows are appended to the buffer with a minimum interval of 100ms (`MinAppendIntervalMs`)
- Balance monitoring and price feed updates run independently in separate goroutines
- The strategy engine queries data every 5 seconds (configurable in `main.go:99`)
