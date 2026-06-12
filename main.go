package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	baseURL   = "https://openapi.tossinvest.com"
	batchSize = 200
)

type config struct {
	ListenAddr   string
	ClientID     string
	ClientSecret string
	Symbols      []string
	MarketFilter string
	StockInfoTTL time.Duration
	PriceRPS     int
	StockRPS     int
}

type apiClient struct {
	httpClient   *http.Client
	clientID     string
	clientSecret string

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

type envelope[T any] struct {
	Result T `json:"result"`
}

type stockInfo struct {
	Symbol              string          `json:"symbol"`
	Name                string          `json:"name"`
	EnglishName         string          `json:"englishName"`
	Market              string          `json:"market"`
	SecurityType        string          `json:"securityType"`
	IsCommonShare       bool            `json:"isCommonShare"`
	Status              string          `json:"status"`
	Currency            string          `json:"currency"`
	SharesOutstanding   json.Number     `json:"sharesOutstanding"`
	KoreanMarketDetails *krMarketDetail `json:"koreanMarketDetail"`
}

type krMarketDetail struct {
	LiquidationTrading  bool  `json:"liquidationTrading"`
	NXTSupported        bool  `json:"nxtSupported"`
	KRXTradingSuspended bool  `json:"krxTradingSuspended"`
	NXTTradingSuspended *bool `json:"nxtTradingSuspended"`
}

type priceResponse struct {
	Symbol    string      `json:"symbol"`
	Timestamp *time.Time  `json:"timestamp"`
	LastPrice json.Number `json:"lastPrice"`
	Currency  string      `json:"currency"`
}

type exchangeRateResponse struct {
	BaseCurrency  string      `json:"baseCurrency"`
	QuoteCurrency string      `json:"quoteCurrency"`
	Rate          json.Number `json:"rate"`
	MidRate       json.Number `json:"midRate"`
	BasisPoint    json.Number `json:"basisPoint"`
	ValidUntil    time.Time   `json:"validUntil"`
}

type exporter struct {
	client       *apiClient
	symbols      []string
	marketFilter string
	stockLimiter *time.Ticker
	priceLimiter *time.Ticker

	stockInfoTTL time.Duration
	stockMu      sync.Mutex
	stockFetched time.Time
	stocks       []stockInfo
	priceSymbols []string

	priceLast      *prometheus.Desc
	priceTimestamp *prometheus.Desc
	stockInfo      *prometheus.Desc
	shares         *prometheus.Desc
	krxSuspended   *prometheus.Desc
	nxtSuspended   *prometheus.Desc
	liquidation    *prometheus.Desc
	exchangeRate   *prometheus.Desc
	exchangeMid    *prometheus.Desc
	exchangeBP     *prometheus.Desc
	exchangeValid  *prometheus.Desc
	scrapeSuccess  *prometheus.Desc
	scrapeDuration *prometheus.Desc
}

func main() {
	loadDotEnv(".env")
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("configuration error", "err", err)
		os.Exit(1)
	}

	client := &apiClient{
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
	}

	exp := newExporter(client, cfg)
	prometheus.MustRegister(exp)

	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	slog.Info("starting tossinvest exporter", "addr", cfg.ListenAddr, "symbols", len(cfg.Symbols), "market_filter", cfg.MarketFilter)
	if err := http.ListenAndServe(cfg.ListenAddr, nil); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}

func loadConfig() (config, error) {
	cfg := config{
		ListenAddr:   getenv("TOSSINVEST_LISTEN_ADDR", ":9108"),
		ClientID:     getenvAny("TOSSINVEST_CLIENT_ID", "client_id"),
		ClientSecret: getenvAny("TOSSINVEST_CLIENT_SECRET", "client_secret"),
		MarketFilter: strings.TrimSpace(os.Getenv("TOSSINVEST_MARKET_FILTER")),
		StockInfoTTL: getenvDuration("TOSSINVEST_STOCK_INFO_TTL", 24*time.Hour),
		PriceRPS:     getenvInt("TOSSINVEST_PRICE_RPS", 10),
		StockRPS:     getenvInt("TOSSINVEST_STOCK_RPS", 5),
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return cfg, errors.New("TOSSINVEST_CLIENT_ID and TOSSINVEST_CLIENT_SECRET are required")
	}

	symbols, err := loadSymbols()
	if err != nil {
		return cfg, err
	}
	if len(symbols) == 0 {
		return cfg, errors.New("provide symbols with TOSSINVEST_SYMBOLS or TOSSINVEST_SYMBOLS_FILE")
	}
	cfg.Symbols = symbols
	if cfg.PriceRPS <= 0 || cfg.StockRPS <= 0 {
		return cfg, errors.New("rate limits must be positive")
	}
	return cfg, nil
}

func loadSymbols() ([]string, error) {
	seen := map[string]bool{}
	var symbols []string
	add := func(raw string) {
		for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\r' || r == '\n' }) {
			symbol := strings.ToUpper(strings.TrimSpace(part))
			if symbol == "" || strings.HasPrefix(symbol, "#") || seen[symbol] {
				continue
			}
			seen[symbol] = true
			symbols = append(symbols, symbol)
		}
	}

	add(os.Getenv("TOSSINVEST_SYMBOLS"))
	for _, file := range strings.Split(os.Getenv("TOSSINVEST_SYMBOLS_FILE"), ",") {
		file = strings.TrimSpace(file)
		if file == "" {
			continue
		}
		f, err := os.Open(file)
		if err != nil {
			return nil, fmt.Errorf("open symbols file: %w", err)
		}

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if beforeComment, _, ok := strings.Cut(line, "#"); ok {
				line = strings.TrimSpace(beforeComment)
			}
			if strings.HasPrefix(line, "#") {
				continue
			}
			add(line)
		}
		_ = f.Close()
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("read symbols file: %w", err)
		}
	}
	return symbols, nil
}

func newExporter(client *apiClient, cfg config) *exporter {
	return &exporter{
		client:       client,
		symbols:      cfg.Symbols,
		marketFilter: cfg.MarketFilter,
		stockLimiter: time.NewTicker(time.Second / time.Duration(cfg.StockRPS)),
		priceLimiter: time.NewTicker(time.Second / time.Duration(cfg.PriceRPS)),
		stockInfoTTL: cfg.StockInfoTTL,
		priceLast: prometheus.NewDesc(
			"tossinvest_price_last",
			"Last traded price from Toss Securities Open API.",
			[]string{"symbol", "currency"}, nil,
		),
		priceTimestamp: prometheus.NewDesc(
			"tossinvest_price_timestamp_seconds",
			"Timestamp of the latest price data from Toss Securities Open API.",
			[]string{"symbol"}, nil,
		),
		stockInfo: prometheus.NewDesc(
			"tossinvest_stock_info",
			"Stock metadata from Toss Securities Open API.",
			[]string{"symbol", "name", "english_name", "market", "security_type", "status", "currency", "is_common_share"}, nil,
		),
		shares: prometheus.NewDesc(
			"tossinvest_stock_shares_outstanding",
			"Shares outstanding from Toss Securities Open API.",
			[]string{"symbol"}, nil,
		),
		krxSuspended: prometheus.NewDesc(
			"tossinvest_stock_krx_trading_suspended",
			"Whether KRX trading is suspended for the stock.",
			[]string{"symbol"}, nil,
		),
		nxtSuspended: prometheus.NewDesc(
			"tossinvest_stock_nxt_trading_suspended",
			"Whether NXT trading is suspended for the stock. Missing NXT support is exported as NaN.",
			[]string{"symbol"}, nil,
		),
		liquidation: prometheus.NewDesc(
			"tossinvest_stock_liquidation_trading",
			"Whether the stock is under liquidation trading.",
			[]string{"symbol"}, nil,
		),
		exchangeRate: prometheus.NewDesc(
			"tossinvest_exchange_rate",
			"Reference exchange rate from Toss Securities Open API.",
			[]string{"base_currency", "quote_currency"}, nil,
		),
		exchangeMid: prometheus.NewDesc(
			"tossinvest_exchange_mid_rate",
			"Reference exchange mid rate from Toss Securities Open API.",
			[]string{"base_currency", "quote_currency"}, nil,
		),
		exchangeBP: prometheus.NewDesc(
			"tossinvest_exchange_basis_point",
			"Basis points of the reference exchange rate against mid rate.",
			[]string{"base_currency", "quote_currency"}, nil,
		),
		exchangeValid: prometheus.NewDesc(
			"tossinvest_exchange_valid_until_seconds",
			"Unix timestamp until which the exchange rate is valid.",
			[]string{"base_currency", "quote_currency"}, nil,
		),
		scrapeSuccess: prometheus.NewDesc(
			"tossinvest_scrape_success",
			"Whether the last Toss Securities Open API scrape succeeded.",
			nil, nil,
		),
		scrapeDuration: prometheus.NewDesc(
			"tossinvest_scrape_duration_seconds",
			"Duration of the Toss Securities Open API scrape.",
			nil, nil,
		),
	}
}

func (e *exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.priceLast
	ch <- e.priceTimestamp
	ch <- e.stockInfo
	ch <- e.shares
	ch <- e.krxSuspended
	ch <- e.nxtSuspended
	ch <- e.liquidation
	ch <- e.exchangeRate
	ch <- e.exchangeMid
	ch <- e.exchangeBP
	ch <- e.exchangeValid
	ch <- e.scrapeSuccess
	ch <- e.scrapeDuration
}

func (e *exporter) Collect(ch chan<- prometheus.Metric) {
	start := time.Now()
	success := 1.0
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()

	stocks, symbols, err := e.getStockUniverse(ctx)
	if err != nil {
		slog.Error("stock info scrape failed", "err", err)
		success = 0
	} else {
		e.emitStocks(ch, stocks)
	}

	prices, err := e.fetchPrices(ctx, symbols)
	if err != nil {
		slog.Error("price scrape failed", "err", err)
		success = 0
	} else {
		e.emitPrices(ch, prices)
	}

	exchange, err := e.fetchExchangeRate(ctx)
	if err != nil {
		slog.Error("exchange rate scrape failed", "err", err)
		success = 0
	} else {
		e.emitExchangeRate(ch, exchange)
	}

	ch <- prometheus.MustNewConstMetric(e.scrapeSuccess, prometheus.GaugeValue, success)
	ch <- prometheus.MustNewConstMetric(e.scrapeDuration, prometheus.GaugeValue, time.Since(start).Seconds())
}

func (e *exporter) getStockUniverse(ctx context.Context) ([]stockInfo, []string, error) {
	e.stockMu.Lock()
	defer e.stockMu.Unlock()

	if time.Since(e.stockFetched) < e.stockInfoTTL && len(e.priceSymbols) > 0 {
		return e.stocks, e.priceSymbols, nil
	}

	stocks, err := e.fetchStocks(ctx, e.symbols)
	if err != nil {
		if len(e.priceSymbols) > 0 {
			return e.stocks, e.priceSymbols, nil
		}
		return nil, e.symbols, err
	}

	var filtered []stockInfo
	var priceSymbols []string
	for _, stock := range stocks {
		if e.marketFilter != "" && !strings.EqualFold(stock.Market, e.marketFilter) {
			continue
		}
		filtered = append(filtered, stock)
		priceSymbols = append(priceSymbols, stock.Symbol)
	}
	if len(priceSymbols) == 0 && e.marketFilter == "" {
		priceSymbols = e.symbols
	}

	e.stocks = filtered
	e.priceSymbols = priceSymbols
	e.stockFetched = time.Now()
	return filtered, priceSymbols, nil
}

func (e *exporter) fetchStocks(ctx context.Context, symbols []string) ([]stockInfo, error) {
	var all []stockInfo
	for _, batch := range batches(symbols, batchSize) {
		stocks, err := e.fetchStockBatch(ctx, batch)
		if err != nil {
			return nil, err
		}
		all = append(all, stocks...)
	}
	return all, nil
}

func (e *exporter) fetchStockBatch(ctx context.Context, symbols []string) ([]stockInfo, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.stockLimiter.C:
	}

	var resp envelope[[]stockInfo]
	values := url.Values{"symbols": {strings.Join(symbols, ",")}}
	if err := e.client.getJSON(ctx, "/api/v1/stocks?"+values.Encode(), &resp); err != nil {
		if len(symbols) == 1 {
			slog.Warn("skipping unsupported stock symbol", "symbol", symbols[0], "err", err)
			return nil, nil
		}
		mid := len(symbols) / 2
		left, leftErr := e.fetchStockBatch(ctx, symbols[:mid])
		if leftErr != nil {
			return nil, leftErr
		}
		right, rightErr := e.fetchStockBatch(ctx, symbols[mid:])
		if rightErr != nil {
			return nil, rightErr
		}
		return append(left, right...), nil
	}
	return resp.Result, nil
}

func (e *exporter) fetchPrices(ctx context.Context, symbols []string) ([]priceResponse, error) {
	var all []priceResponse
	for _, batch := range batches(symbols, batchSize) {
		prices, err := e.fetchPriceBatch(ctx, batch)
		if err != nil {
			return nil, err
		}
		all = append(all, prices...)
	}
	return all, nil
}

func (e *exporter) fetchPriceBatch(ctx context.Context, symbols []string) ([]priceResponse, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.priceLimiter.C:
	}

	var resp envelope[[]priceResponse]
	values := url.Values{"symbols": {strings.Join(symbols, ",")}}
	if err := e.client.getJSON(ctx, "/api/v1/prices?"+values.Encode(), &resp); err != nil {
		if len(symbols) == 1 {
			slog.Warn("skipping unsupported price symbol", "symbol", symbols[0], "err", err)
			return nil, nil
		}
		mid := len(symbols) / 2
		left, leftErr := e.fetchPriceBatch(ctx, symbols[:mid])
		if leftErr != nil {
			return nil, leftErr
		}
		right, rightErr := e.fetchPriceBatch(ctx, symbols[mid:])
		if rightErr != nil {
			return nil, rightErr
		}
		return append(left, right...), nil
	}
	return resp.Result, nil
}

func (e *exporter) fetchExchangeRate(ctx context.Context) (exchangeRateResponse, error) {
	var resp envelope[exchangeRateResponse]
	values := url.Values{
		"baseCurrency":  {"USD"},
		"quoteCurrency": {"KRW"},
	}
	if err := e.client.getJSON(ctx, "/api/v1/exchange-rate?"+values.Encode(), &resp); err != nil {
		return exchangeRateResponse{}, err
	}
	return resp.Result, nil
}

func (e *exporter) emitStocks(ch chan<- prometheus.Metric, stocks []stockInfo) {
	for _, stock := range stocks {
		ch <- prometheus.MustNewConstMetric(e.stockInfo, prometheus.GaugeValue, 1, stock.Symbol, stock.Name, stock.EnglishName, stock.Market, stock.SecurityType, stock.Status, stock.Currency, strconv.FormatBool(stock.IsCommonShare))
		if shares, ok := numberFloat(stock.SharesOutstanding); ok {
			ch <- prometheus.MustNewConstMetric(e.shares, prometheus.GaugeValue, shares, stock.Symbol)
		}
		if stock.KoreanMarketDetails == nil {
			continue
		}
		ch <- prometheus.MustNewConstMetric(e.krxSuspended, prometheus.GaugeValue, boolFloat(stock.KoreanMarketDetails.KRXTradingSuspended), stock.Symbol)
		ch <- prometheus.MustNewConstMetric(e.liquidation, prometheus.GaugeValue, boolFloat(stock.KoreanMarketDetails.LiquidationTrading), stock.Symbol)
		if stock.KoreanMarketDetails.NXTTradingSuspended != nil {
			ch <- prometheus.MustNewConstMetric(e.nxtSuspended, prometheus.GaugeValue, boolFloat(*stock.KoreanMarketDetails.NXTTradingSuspended), stock.Symbol)
		}
	}
}

func (e *exporter) emitPrices(ch chan<- prometheus.Metric, prices []priceResponse) {
	for _, price := range prices {
		last, ok := numberFloat(price.LastPrice)
		if !ok {
			continue
		}
		ch <- prometheus.MustNewConstMetric(e.priceLast, prometheus.GaugeValue, last, price.Symbol, price.Currency)
		if price.Timestamp != nil {
			ch <- prometheus.MustNewConstMetric(e.priceTimestamp, prometheus.GaugeValue, float64(price.Timestamp.Unix()), price.Symbol)
		}
	}
}

func (e *exporter) emitExchangeRate(ch chan<- prometheus.Metric, exchange exchangeRateResponse) {
	labels := []string{exchange.BaseCurrency, exchange.QuoteCurrency}
	if rate, ok := numberFloat(exchange.Rate); ok {
		ch <- prometheus.MustNewConstMetric(e.exchangeRate, prometheus.GaugeValue, rate, labels...)
	}
	if midRate, ok := numberFloat(exchange.MidRate); ok {
		ch <- prometheus.MustNewConstMetric(e.exchangeMid, prometheus.GaugeValue, midRate, labels...)
	}
	if basisPoint, ok := numberFloat(exchange.BasisPoint); ok {
		ch <- prometheus.MustNewConstMetric(e.exchangeBP, prometheus.GaugeValue, basisPoint, labels...)
	}
	if !exchange.ValidUntil.IsZero() {
		ch <- prometheus.MustNewConstMetric(e.exchangeValid, prometheus.GaugeValue, float64(exchange.ValidUntil.Unix()), labels...)
	}
}

func (c *apiClient) getJSON(ctx context.Context, path string, out any) error {
	token, err := c.token(ctx)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("toss api %s failed: status=%d body=%s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	decoder := json.NewDecoder(resp.Body)
	decoder.UseNumber()
	return decoder.Decode(out)
}

func (c *apiClient) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.accessToken != "" && time.Until(c.expiresAt) > time.Minute {
		token := c.accessToken
		c.mu.Unlock()
		return token, nil
	}
	c.mu.Unlock()

	values := url.Values{}
	values.Set("grant_type", "client_credentials")
	values.Set("client_id", c.clientID)
	values.Set("client_secret", c.clientSecret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/oauth2/token", bytes.NewBufferString(values.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("token request failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var token tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&token); err != nil {
		return "", err
	}
	if token.AccessToken == "" {
		return "", errors.New("token response did not include access_token")
	}
	if token.ExpiresIn == 0 {
		token.ExpiresIn = 3600
	}

	c.mu.Lock()
	c.accessToken = token.AccessToken
	c.expiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	c.mu.Unlock()

	return token.AccessToken, nil
}

func batches(symbols []string, size int) [][]string {
	var out [][]string
	for start := 0; start < len(symbols); start += size {
		end := start + size
		if end > len(symbols) {
			end = len(symbols)
		}
		out = append(out, symbols[start:end])
	}
	return out
}

func numberFloat(n json.Number) (float64, bool) {
	if n.String() == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(n.String(), 64)
	return v, err == nil
}

func boolFloat(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

func getenv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func getenvAny(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func getenvInt(key string, fallback int) int {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		parsed, err := strconv.Atoi(value)
		if err == nil {
			return parsed
		}
	}
	return fallback
}

func getenvDuration(key string, fallback time.Duration) time.Duration {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		parsed, err := time.ParseDuration(value)
		if err == nil {
			return parsed
		}
	}
	return fallback
}

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), "\"'")
		if key != "" && os.Getenv(key) == "" {
			_ = os.Setenv(key, value)
		}
	}
}
