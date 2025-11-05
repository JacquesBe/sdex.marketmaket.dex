# Bot Configuration Reference

This document describes all configuration fields in the MongoDB `BotConfig` document.

---

## Core Configuration

### `publicKey`
- **Type:** `string`
- **Unit:** Stellar public key (G...)
- **Usage:** The bot's Stellar account public key for making trades

### `secretKey`
- **Type:** `string`
- **Unit:** Stellar secret key (S...)
- **Usage:** The bot's Stellar account secret key (stored encrypted in MongoDB)

### `baseAsset`
- **Type:** `string`
- **Unit:** Asset code (e.g., "XLM")
- **Usage:** The base asset being traded (what you're buying/selling)

### `counterAsset`
- **Type:** `string`
- **Unit:** Asset code (e.g., "SHX", "USDC")
- **Usage:** The counter/quote asset (what you're pricing the base asset in)

### `baseAssetIssuer`
- **Type:** `string`
- **Unit:** Stellar public key or "native"
- **Usage:** Issuer of the base asset. Use "native" for XLM

### `counterAssetIssuer`
- **Type:** `string`
- **Unit:** Stellar public key or "native"
- **Usage:** Issuer of the counter asset. Use "native" for XLM

### `minOrderbookAmount`
- **Type:** `float64`
- **Unit:** Base asset units (e.g., XLM)
- **Usage:** Minimum L1 (best bid/ask) amount to accept from orderbook. If L1 order is below this threshold, it's removed before calculating mid price and features. Set to `0` to disable filtering.
- **Example:** `100.0` = ignore L1 orders < 100 XLM

---

## Price Feed Options (`priceFeedOptions`)

### `baseAsset`
- **Type:** `string`
- **Unit:** CEX asset code (e.g., "XLM")
- **Usage:** Base asset symbol for the external price feed (Binance/Horizon)

### `counterAsset`
- **Type:** `string`
- **Unit:** CEX asset code (e.g., "EUR", "USDT")
- **Usage:** Counter asset symbol for the external price feed

### `inverse`
- **Type:** `bool`
- **Unit:** boolean
- **Usage:** Whether to invert the price feed (e.g., if feed is EUR/XLM but you trade XLM/EUR)

### `priceFeedMultiple`
- **Type:** `float64`
- **Unit:** Multiplier (dimensionless)
- **Usage:** Multiplier applied to external prices before using them (e.g., `1.02` = 2% markup)

### `bufferLength`
- **Type:** `int`
- **Unit:** Number of feature rows
- **Usage:** Size of the circular buffer storing historical feature rows for volatility calculation

### `bookDepth`
- **Type:** `int`
- **Unit:** Number of orderbook levels (5, 10, or 20)
- **Usage:** How many depth levels to fetch from the orderbook for imbalance calculations

### `orderBookTickRate`
- **Type:** `int`
- **Unit:** Milliseconds
- **Usage:** Polling interval for fetching orderbook snapshots (Horizon only)
- **Example:** `3000` = poll every 3 seconds

### `sseTimeoutSeconds`
- **Type:** `int`
- **Unit:** Seconds
- **Usage:** HTTP timeout for orderbook REST requests (Horizon only)
- **Example:** `60` = 60 second timeout

---

## Balance Monitor Options (`balanceMonitorOptions`)

### `tickRate`
- **Type:** `int`
- **Unit:** Milliseconds
- **Usage:** How often to fetch account balances from Horizon
- **Example:** `5000` = check balances every 5 seconds

---

## Strategy Options (`strategyOptions`)

### `volWindowMs`
- **Type:** `int64`
- **Unit:** Milliseconds
- **Usage:** Time window for calculating rolling volatility
- **Example:** `30000` = 30 second volatility window

### `blendWeight`
- **Type:** `float64`
- **Unit:** Weight between 0 and 1
- **Usage:** Weight for microprice vs external mid price in fair price calculation
- **Formula:** `fairPrice = (1 - blendWeight) * externalMid + blendWeight * microPrice`
- **Example:** `0.3` = 30% microprice, 70% external mid

### `riskAversion`
- **Type:** `float64`
- **Unit:** Dimensionless coefficient
- **Usage:** Risk aversion parameter (currently unused in strategy)

### `halfSpreadFloor`
- **Type:** `float64`
- **Unit:** Basis points (bps)
- **Usage:** Minimum half-spread around fair price, in basis points
- **Example:** `10.0` = minimum 10 bps half-spread (20 bps total spread)

### `volatilitySensitivity`
- **Type:** `float64`
- **Unit:** Multiplier (dimensionless)
- **Usage:** How much rolling volatility widens spreads
- **Formula:** `volComponent = volatilitySensitivity * rollingVol`
- **Example:** `2.0` = 2x volatility multiplier

### `inventoryBias`
- **Type:** `float64`
- **Unit:** Multiplier (dimensionless)
- **Usage:** How much inventory imbalance asymmetrically widens spreads
- **Formula:** 
  - `bidAdjustment = inventoryBias * max(0, relativeInventory) * fairPrice`
  - `askAdjustment = inventoryBias * max(0, -relativeInventory) * fairPrice`
- **Example:** `0.5` = moderate inventory penalty

### `obImbalanceSensitivity`
- **Type:** `float64`
- **Unit:** Multiplier (dimensionless)
- **Usage:** How much orderbook imbalance asymmetrically widens spreads
- **Formula:**
  - `bidPenalty = obImbalanceSensitivity * max(0, -depthImbalance) * fairPrice`
  - `askPenalty = obImbalanceSensitivity * max(0, depthImbalance) * fairPrice`
- **Example:** `0.3` = moderate OB imbalance penalty

### `priceSmoothing`
- **Type:** `float64`
- **Unit:** EWMA alpha (0 to 1)
- **Usage:** Exponential smoothing factor for fair price to prevent manipulation
- **Formula:** `ewmaPrice = alpha * instantPrice + (1 - alpha) * previousEwma`
- **Example:** 
  - `0.2` = slow smoothing (80% historical)
  - `0.5` = balanced
  - `1.0` = no smoothing (instant price)

---

## Offer Manager Options (`offerManagerOptions`)

### `offerMonitorTickRate`
- **Type:** `int`
- **Unit:** Milliseconds
- **Usage:** How often to check and update offers on SDEX
- **Example:** `10000` = update offers every 10 seconds

### `offerQuantity`
- **Type:** `float64`
- **Unit:** Base asset units
- **Usage:** Size of each bid/ask offer placed on SDEX
- **Example:** `100.0` = place 100 XLM per side

### `priceGraceBasisPoints`
- **Type:** `float64`
- **Unit:** Basis points (bps)
- **Usage:** Price tolerance before updating an offer
- **Example:** `5.0` = don't update if price within 5 bps of target

### `offerGraceQuantity`
- **Type:** `float64`
- **Unit:** Base asset units
- **Usage:** Quantity tolerance before updating an offer
- **Example:** `10.0` = don't update if quantity within 10 XLM of target

### `feeMaxStroops`
- **Type:** `int64`
- **Unit:** Stroops (1 stroop = 0.0000001 XLM)
- **Usage:** Maximum transaction fee willing to pay per operation
- **Example:** `100000` = max 0.01 XLM fee
