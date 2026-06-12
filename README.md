# tossinvest-exporter

Prometheus exporter for Toss Securities Open API market data.

## Configuration

Required:

```sh
export TOSSINVEST_CLIENT_ID='your-client-id'
export TOSSINVEST_CLIENT_SECRET='your-client-secret'
export TOSSINVEST_SYMBOLS_FILE='./symbols.txt'
```

`symbols.txt` can contain comma-separated symbols or one symbol per line:

```text
005930
000660
035420
```

Optional:

```sh
export TOSSINVEST_LISTEN_ADDR=':9108'
export TOSSINVEST_SYMBOLS='005930,000660,AAPL'
export TOSSINVEST_MARKET_FILTER='KOSPI'
export TOSSINVEST_STOCK_INFO_TTL='24h'
export TOSSINVEST_PRICE_RPS='10'
export TOSSINVEST_STOCK_RPS='5'
```

Toss can fetch prices for up to 200 symbols per request. The exporter batches symbols at that size and rate-limits requests. Toss does not document an endpoint that enumerates all KOSPI symbols, so provide the symbol universe with `TOSSINVEST_SYMBOLS_FILE` or `TOSSINVEST_SYMBOLS`.

You can generate `symbols.txt` from Naver Finance market-cap listing pages:

```sh
go run ./cmd/fetch-symbols -market kospi -out symbols.txt
```

For KOSDAQ:

```sh
go run ./cmd/fetch-symbols -market kosdaq -out symbols-kosdaq.txt
```

For KOSPI + KOSDAQ:

```sh
go run ./cmd/fetch-symbols -market kr -out symbols-kr.txt
```

For US exchanges, symbols are generated from Nasdaq Trader symbol directory files:

```sh
go run ./cmd/fetch-symbols -market nasdaq -out symbols-nasdaq.txt
go run ./cmd/fetch-symbols -market nyse -out symbols-nyse.txt
go run ./cmd/fetch-symbols -market us -out symbols-us.txt
```

For cleaner US stock lists that exclude ETFs, warrants, rights, units, and preferreds:

```sh
go run ./cmd/fetch-symbols -market nasdaq -stocks-only -out symbols-nasdaq-stocks.txt
go run ./cmd/fetch-symbols -market nyse -stocks-only -out symbols-nyse-stocks.txt
go run ./cmd/fetch-symbols -market us -stocks-only -out symbols-us-stocks.txt
```

For KOSPI + KOSDAQ + cleaner US stocks in one file:

```sh
go run ./cmd/fetch-symbols -market all-stocks -out symbols-all-stocks.txt
```

To scrape multiple files in one exporter process, pass comma-separated files:

```sh
export TOSSINVEST_SYMBOLS_FILE='symbols.txt,symbols-nasdaq-stocks.txt,symbols-nyse-stocks.txt'
```

## Run

```sh
go run ./...
```

Then scrape:

```text
http://localhost:9108/metrics
```

## Metrics

- `tossinvest_price_last{symbol,currency}`
- `tossinvest_price_timestamp_seconds{symbol}`
- `tossinvest_stock_info{symbol,name,english_name,market,security_type,status,currency,is_common_share}`
- `tossinvest_stock_shares_outstanding{symbol}`
- `tossinvest_stock_krx_trading_suspended{symbol}`
- `tossinvest_stock_nxt_trading_suspended{symbol}`
- `tossinvest_stock_liquidation_trading{symbol}`
- `tossinvest_exchange_rate{base_currency,quote_currency}`
- `tossinvest_exchange_mid_rate{base_currency,quote_currency}`
- `tossinvest_exchange_basis_point{base_currency,quote_currency}`
- `tossinvest_exchange_valid_until_seconds{base_currency,quote_currency}`
- `tossinvest_scrape_success`
- `tossinvest_scrape_duration_seconds`
