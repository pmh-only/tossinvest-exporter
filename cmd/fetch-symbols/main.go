package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	naverMarketSumURL = "https://finance.naver.com/sise/sise_market_sum.naver"
	nasdaqListedURL   = "https://www.nasdaqtrader.com/dynamic/SymDir/nasdaqlisted.txt"
	otherListedURL    = "https://www.nasdaqtrader.com/dynamic/SymDir/otherlisted.txt"
)

var (
	stockCodeRE = regexp.MustCompile(`/item/main\.naver\?code=(\d{6})" class="tltle"`)
	lastPageRE  = regexp.MustCompile(`sise_market_sum\.naver\?sosok=\d+&amp;page=(\d+)"[^>]*>[^<]*`) // includes the pgRR last-page link.
)

func main() {
	market := flag.String("market", "kospi", "market to fetch: kospi, kosdaq, kr, nasdaq, nyse, nyse-american, us, all-stocks")
	out := flag.String("out", "symbols.txt", "output symbols file")
	sleep := flag.Duration("sleep", 150*time.Millisecond, "delay between paginated page requests")
	stocksOnly := flag.Bool("stocks-only", false, "for US markets, exclude ETFs, warrants, rights, units, and preferreds")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client := &http.Client{Timeout: 20 * time.Second}
	symbols, err := fetchMarketSymbols(ctx, client, *market, *sleep, *stocksOnly)
	if err != nil {
		slog.Error("fetch symbols failed", "err", err)
		os.Exit(1)
	}
	if len(symbols) == 0 {
		slog.Error("no symbols found")
		os.Exit(1)
	}

	if err := writeSymbols(*out, symbols); err != nil {
		slog.Error("write symbols failed", "err", err)
		os.Exit(1)
	}
	slog.Info("wrote symbols", "market", strings.ToUpper(*market), "count", len(symbols), "out", *out)
}

func fetchMarketSymbols(ctx context.Context, client *http.Client, market string, sleep time.Duration, stocksOnly bool) ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(market)) {
	case "kospi", "kosdaq":
		sosok, err := marketSosok(market)
		if err != nil {
			return nil, err
		}
		return fetchKoreanSymbols(ctx, client, sosok, sleep)
	case "kr", "korea":
		return combineSymbolSets(
			func() ([]string, error) { return fetchKoreanSymbols(ctx, client, 0, sleep) },
			func() ([]string, error) { return fetchKoreanSymbols(ctx, client, 1, sleep) },
		)
	case "nasdaq":
		return fetchNASDAQSymbols(ctx, client, stocksOnly)
	case "nyse":
		return fetchOtherListedSymbols(ctx, client, map[string]bool{"N": true}, stocksOnly)
	case "nyse-american", "amex":
		return fetchOtherListedSymbols(ctx, client, map[string]bool{"A": true}, stocksOnly)
	case "us":
		return fetchUSSymbols(ctx, client, stocksOnly)
	case "all-stocks", "all":
		return combineSymbolSets(
			func() ([]string, error) { return fetchKoreanSymbols(ctx, client, 0, sleep) },
			func() ([]string, error) { return fetchKoreanSymbols(ctx, client, 1, sleep) },
			func() ([]string, error) { return fetchUSSymbols(ctx, client, true) },
		)
	default:
		return nil, fmt.Errorf("unsupported market %q", market)
	}
}

func combineSymbolSets(fetchers ...func() ([]string, error)) ([]string, error) {
	seen := map[string]bool{}
	for _, fetch := range fetchers {
		symbols, err := fetch()
		if err != nil {
			return nil, err
		}
		for _, symbol := range symbols {
			seen[symbol] = true
		}
	}
	return sortedSymbols(seen), nil
}

func marketSosok(market string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(market)) {
	case "kospi":
		return 0, nil
	case "kosdaq":
		return 1, nil
	default:
		return 0, fmt.Errorf("unsupported Korean market %q", market)
	}
}

func fetchKoreanSymbols(ctx context.Context, client *http.Client, sosok int, sleep time.Duration) ([]string, error) {
	firstPage, err := fetchPage(ctx, client, sosok, 1)
	if err != nil {
		return nil, err
	}
	lastPage := parseLastPage(firstPage)
	if lastPage == 0 {
		return nil, errors.New("could not determine last page")
	}

	seen := map[string]bool{}
	for _, symbol := range parseSymbols(firstPage) {
		seen[symbol] = true
	}

	for page := 2; page <= lastPage; page++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(sleep):
		}

		body, err := fetchPage(ctx, client, sosok, page)
		if err != nil {
			return nil, err
		}
		for _, symbol := range parseSymbols(body) {
			seen[symbol] = true
		}
	}

	return sortedSymbols(seen), nil
}

func fetchPage(ctx context.Context, client *http.Client, sosok, page int) (string, error) {
	url := fmt.Sprintf("%s?sosok=%d&page=%d", naverMarketSumURL, sosok, page)
	return fetchText(ctx, client, url, "text/html")
}

func parseSymbols(body string) []string {
	matches := stockCodeRE.FindAllStringSubmatch(body, -1)
	symbols := make([]string, 0, len(matches))
	for _, match := range matches {
		symbols = append(symbols, match[1])
	}
	return symbols
}

func parseLastPage(body string) int {
	matches := lastPageRE.FindAllStringSubmatch(body, -1)
	last := 1
	for _, match := range matches {
		page, err := strconv.Atoi(match[1])
		if err == nil && page > last {
			last = page
		}
	}
	return last
}

func fetchNASDAQSymbols(ctx context.Context, client *http.Client, stocksOnly bool) ([]string, error) {
	body, err := fetchText(ctx, client, nasdaqListedURL, "text/plain")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "|")
		if len(fields) < 4 || fields[0] == "Symbol" || strings.HasPrefix(fields[0], "File Creation Time") {
			continue
		}
		if fields[3] != "N" || (stocksOnly && !isLikelyStock(fields[1], fields[6])) {
			continue
		}
		if symbol := normalizeUSSymbol(fields[0]); symbol != "" {
			seen[symbol] = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return sortedSymbols(seen), nil
}

func fetchOtherListedSymbols(ctx context.Context, client *http.Client, exchanges map[string]bool, stocksOnly bool) ([]string, error) {
	body, err := fetchText(ctx, client, otherListedURL, "text/plain")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "|")
		if len(fields) < 7 || fields[0] == "ACT Symbol" || strings.HasPrefix(fields[0], "File Creation Time") {
			continue
		}
		if !exchanges[fields[2]] || fields[6] != "N" || (stocksOnly && !isLikelyStock(fields[1], fields[4])) {
			continue
		}
		if symbol := normalizeUSSymbol(fields[0]); symbol != "" {
			seen[symbol] = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return sortedSymbols(seen), nil
}

func fetchUSSymbols(ctx context.Context, client *http.Client, stocksOnly bool) ([]string, error) {
	nasdaq, err := fetchNASDAQSymbols(ctx, client, stocksOnly)
	if err != nil {
		return nil, err
	}
	other, err := fetchOtherListedSymbols(ctx, client, map[string]bool{"A": true, "N": true, "P": true, "Z": true, "V": true}, stocksOnly)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, symbol := range nasdaq {
		seen[symbol] = true
	}
	for _, symbol := range other {
		seen[symbol] = true
	}
	return sortedSymbols(seen), nil
}

func isLikelyStock(name string, etf string) bool {
	if etf == "Y" {
		return false
	}
	lower := strings.ToLower(name)
	excluded := []string{
		" warrant",
		" warrants",
		" right",
		" rights",
		" unit",
		" units",
		" preferred",
		" notes",
		" note ",
		" etf",
		" etn",
		" fund",
	}
	for _, word := range excluded {
		if strings.Contains(lower, word) {
			return false
		}
	}
	return true
}

func normalizeUSSymbol(symbol string) string {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" || strings.Contains(symbol, "$") {
		return ""
	}
	return strings.ReplaceAll(symbol, ".", "-")
}

func fetchText(ctx context.Context, client *http.Client, url string, accept string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "tossinvest-exporter/0.1 (+https://github.com/pmh-only/tossinvest-exporter)")
	req.Header.Set("Accept", accept)

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetch %s failed: status=%d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func sortedSymbols(seen map[string]bool) []string {
	symbols := make([]string, 0, len(seen))
	for symbol := range seen {
		symbols = append(symbols, symbol)
	}
	sort.Strings(symbols)
	return symbols
}

func writeSymbols(path string, symbols []string) error {
	return os.WriteFile(path, []byte(strings.Join(symbols, "\n")+"\n"), 0o644)
}
