package loadtest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"example.com/wagering/internal/application"
	"example.com/wagering/internal/domain"
)

type Config struct {
	API, InternalToken, ProviderToken, ProviderID string
	Requests, Workers, Wallets                    int
	Client                                        *http.Client
}
type Report struct {
	Requests         int         `json:"requests"`
	Completed        int         `json:"completed"`
	Workers          int         `json:"workers"`
	Wallets          int         `json:"wallets"`
	ElapsedSeconds   float64     `json:"elapsedSeconds"`
	ThroughputRPS    float64     `json:"throughputRps"`
	P50Milliseconds  float64     `json:"p50Milliseconds"`
	P95Milliseconds  float64     `json:"p95Milliseconds"`
	P99Milliseconds  float64     `json:"p99Milliseconds"`
	Errors           int         `json:"errors"`
	Conflicts        int         `json:"conflicts"`
	StatusCounts     map[int]int `json:"statusCounts"`
	OutboxLagSeconds float64     `json:"outboxLagSeconds"`
}
type outcome struct {
	status   int
	duration time.Duration
	err      error
}

func request(ctx context.Context, client *http.Client, method, url, token, key string, body any) (int, []byte, error) {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(data))
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	data, err = io.ReadAll(io.LimitReader(res.Body, 64*1024))
	return res.StatusCode, data, err
}
func percentile(values []time.Duration, percentage int) float64 {
	if len(values) == 0 {
		return 0
	}
	index := (percentage*len(values)+99)/100 - 1
	return float64(values[index]) / float64(time.Millisecond)
}
func outboxLag(ctx context.Context, client *http.Client, api, token string) (float64, error) {
	status, body, err := request(ctx, client, http.MethodGet, api+"/metrics", token, "", nil)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("metrics returned HTTP %d", status)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "wager_outbox_lag_seconds ") {
			continue
		}
		value := strings.TrimPrefix(line, "wager_outbox_lag_seconds ")
		lag, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(lag) || math.IsInf(lag, 0) || lag < 0 {
			return 0, errors.New("invalid outbox lag metric")
		}
		return lag, nil
	}
	return 0, errors.New("outbox lag metric missing")
}
func Run(ctx context.Context, cfg Config) (Report, error) {
	if cfg.API == "" || cfg.InternalToken == "" || cfg.ProviderToken == "" || cfg.ProviderID == "" || cfg.Requests < 1 || cfg.Requests > 100000 || cfg.Workers < 1 || cfg.Workers > 256 || cfg.Wallets < 1 || cfg.Wallets > 1000 {
		return Report{}, errors.New("invalid load configuration")
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 12 * time.Second}
	}
	base := strings.TrimRight(cfg.API, "/")
	runID, err := application.NewID()
	if err != nil {
		return Report{}, err
	}
	opening, err := domain.ParseMoney("100000.00", "BRL")
	if err != nil {
		return Report{}, err
	}
	betAmount, err := domain.ParseMoney("0.01", "BRL")
	if err != nil {
		return Report{}, err
	}
	wallets := make([]application.WalletView, cfg.Wallets)
	for i := range wallets {
		playerID, err := application.NewID()
		if err != nil {
			return Report{}, err
		}
		status, body, err := request(ctx, client, http.MethodPost, base+"/wallets", cfg.InternalToken, "", map[string]any{"playerId": playerID, "initialBalance": opening})
		if err != nil {
			return Report{}, err
		}
		if status != http.StatusCreated {
			return Report{}, fmt.Errorf("create wallet %d returned HTTP %d", i, status)
		}
		if err := json.Unmarshal(body, &wallets[i]); err != nil {
			return Report{}, err
		}
	}
	jobs := make(chan int)
	results := make(chan outcome)
	var workers sync.WaitGroup
	started := time.Now()
	for n := 0; n < cfg.Workers; n++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				wallet := wallets[i%len(wallets)]
				identity := fmt.Sprintf("load-%s-%d", runID, i)
				operation := domain.Operation{ProviderID: cfg.ProviderID, ExternalTransactionID: identity, PlayerID: wallet.PlayerID, WalletID: wallet.ID, RoundID: "load-round", GameID: "load-game", Kind: "BET", Money: betAmount}
				start := time.Now()
				status, _, err := request(ctx, client, http.MethodPost, base+"/wagering/transactions", cfg.ProviderToken, identity, operation)
				results <- outcome{status: status, duration: time.Since(start), err: err}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for i := 0; i < cfg.Requests; i++ {
			select {
			case jobs <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { workers.Wait(); close(results) }()
	report := Report{Requests: cfg.Requests, Workers: cfg.Workers, Wallets: cfg.Wallets, StatusCounts: map[int]int{}}
	durations := make([]time.Duration, 0, cfg.Requests)
	for result := range results {
		report.Completed++
		durations = append(durations, result.duration)
		if result.err != nil {
			report.Errors++
			continue
		}
		report.StatusCounts[result.status]++
		if result.status == http.StatusConflict {
			report.Conflicts++
		}
		if result.status != http.StatusOK {
			report.Errors++
		}
	}
	elapsed := time.Since(started)
	report.ElapsedSeconds = elapsed.Seconds()
	if report.ElapsedSeconds > 0 {
		report.ThroughputRPS = float64(report.Completed) / report.ElapsedSeconds
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	report.P50Milliseconds = percentile(durations, 50)
	report.P95Milliseconds = percentile(durations, 95)
	report.P99Milliseconds = percentile(durations, 99)
	if err := ctx.Err(); err != nil {
		return report, err
	}
	// The observer updates this gauge every five seconds; keep this wait outside elapsed time.
	timer := time.NewTimer(6 * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return report, ctx.Err()
	}
	report.OutboxLagSeconds, err = outboxLag(ctx, client, base, cfg.InternalToken)
	return report, err
}
