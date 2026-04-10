package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

var (
	pprofCfg            *PprofCaptureConfig
	pprofSeconds        int
	pprofOutDir         string
	pprofCosmosOK       atomic.Uint64
	pprofTxWarmupDone   atomic.Bool // every_n_txs counts only after warmup when WarmupSeconds > 0
)

func expandOutputDir(out string) string {
	if out == "" {
		return "."
	}
	if out == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "."
		}
		return home
	}
	if strings.HasPrefix(out, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return out
		}
		return filepath.Join(home, strings.TrimPrefix(out, "~/"))
	}
	return out
}

func startPprofCaptureIfConfigured(config *Config, done <-chan struct{}) {
	if config.PprofCapture == nil || strings.TrimSpace(config.PprofCapture.BaseURL) == "" {
		return
	}
	cfg := config.PprofCapture
	secs := cfg.Seconds
	if secs <= 0 {
		secs = 10
	}
	out := expandOutputDir(cfg.OutputDir)
	pprofCfg = cfg
	pprofSeconds = secs
	pprofOutDir = out

	if err := os.MkdirAll(out, 0o750); err != nil {
		fmt.Printf("pprof_capture: mkdir %q: %v\n", out, err)
		return
	}

	heights := append([]int64(nil), cfg.BlockHeights...)
	slices.Sort(heights)
	if cfg.WarmupSeconds > 0 && cfg.EveryNTx > 0 {
		pprofTxWarmupDone.Store(false)
		go runPprofWarmupThenEnableTxCount(cfg.WarmupSeconds, done)
	} else {
		pprofTxWarmupDone.Store(true)
	}

	if len(heights) > 0 {
		go runPprofBlockHeightWatcher(cfg, heights, secs, out, config.BlockchainEndpoint, done)
	}

	fmt.Printf("pprof_capture: enabled base_url=%s seconds=%d output_dir=%q block_heights=%v every_n_txs=%d warmup_s=%d steady_before_s=%d poll_ms=%d\n",
		cfg.BaseURL, secs, out, heights, cfg.EveryNTx, cfg.WarmupSeconds, cfg.SteadySecondsBeforeProfile, cfg.HeightPollIntervalMs)
}

func runPprofWarmupThenEnableTxCount(warmupSeconds uint64, done <-chan struct{}) {
	if warmupSeconds == 0 {
		pprofTxWarmupDone.Store(true)
		return
	}
	t := time.NewTimer(time.Duration(warmupSeconds) * time.Second)
	defer t.Stop()
	select {
	case <-done:
		return
	case <-t.C:
		pprofTxWarmupDone.Store(true)
		fmt.Printf("pprof_capture: warmup finished (%ds), tx-based captures can start\n", warmupSeconds)
	}
}

func runPprofBlockHeightWatcher(
	cfg *PprofCaptureConfig,
	milestones []int64,
	seconds int,
	outDir, blockchainEndpoint string,
	done <-chan struct{},
) {
	if cfg.WarmupSeconds > 0 {
		t := time.NewTimer(time.Duration(cfg.WarmupSeconds) * time.Second)
		select {
		case <-done:
			if !t.Stop() {
				<-t.C
			}
			return
		case <-t.C:
		}
		fmt.Printf("pprof_capture: block watcher warmup done (%ds)\n", cfg.WarmupSeconds)
	}

	pollMs := cfg.HeightPollIntervalMs
	if pollMs <= 0 {
		pollMs = 250
	}

	for next := 0; next < len(milestones); {
		select {
		case <-done:
			return
		default:
		}

		h := int64(getLastHeight(blockchainEndpoint))
		target := milestones[next]
		if h < target {
			remaining := target - h
			sleepMs := pollMs
			if remaining > 15 {
				sleepMs = 1000
			} else if remaining > 5 {
				sleepMs = min(pollMs, 500)
			} else {
				sleepMs = min(pollMs, 100)
			}
			t := time.NewTimer(time.Duration(sleepMs) * time.Millisecond)
			select {
			case <-done:
				if !t.Stop() {
					<-t.C
				}
				return
			case <-t.C:
			}
			continue
		}

		// Reached milestone: optional steady window at fixed height regime + steady TPS.
		if cfg.SteadySecondsBeforeProfile > 0 {
			fmt.Printf("pprof_capture: height>=%d reached (h=%d), steady wait %ds before profile\n", target, h, cfg.SteadySecondsBeforeProfile)
			t := time.NewTimer(time.Duration(cfg.SteadySecondsBeforeProfile) * time.Second)
			select {
			case <-done:
				if !t.Stop() {
					<-t.C
				}
				return
			case <-t.C:
			}
		}

		hAt := int64(getLastHeight(blockchainEndpoint))
		tag := fmt.Sprintf("block%d_h%d", target, hAt)
		// Synchronous capture avoids overlapping CPU profiles when multiple milestones are close.
		capturePprofProfile(cfg.BaseURL, seconds, outDir, tag)
		next++
	}
}

func capturePprofProfile(baseURL string, seconds int, outDir, tag string) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	url := fmt.Sprintf("%s/debug/pprof/profile?seconds=%d", baseURL, seconds)
	client := &http.Client{Timeout: time.Duration(seconds+20) * time.Second}
	resp, err := client.Get(url) //nolint:gosec // URL is operator-supplied pprof endpoint
	if err != nil {
		fmt.Printf("pprof_capture: GET %s: %v\n", url, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("pprof_capture: %s -> %s\n", url, resp.Status)
		return
	}
	name := fmt.Sprintf("cpu_%s_%s.pb.gz", tag, time.Now().Format("20060102_150405"))
	path := filepath.Join(outDir, name)
	f, err := os.Create(filepath.Clean(path))
	if err != nil {
		fmt.Printf("pprof_capture: create %s: %v\n", path, err)
		return
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		fmt.Printf("pprof_capture: write %s: %v\n", path, err)
		_ = f.Close()
		return
	}
	if err := f.Close(); err != nil {
		fmt.Printf("pprof_capture: close %s: %v\n", path, err)
		return
	}
	fmt.Printf("pprof_capture: wrote %s\n", path)
}

func maybePprofAfterCosmosSuccess() {
	if pprofCfg == nil || pprofCfg.EveryNTx == 0 {
		return
	}
	if pprofCfg.EveryNTx > 0 && pprofCfg.WarmupSeconds > 0 && !pprofTxWarmupDone.Load() {
		return
	}
	n := pprofCosmosOK.Add(1)
	if n%pprofCfg.EveryNTx != 0 {
		return
	}
	tag := fmt.Sprintf("tx%d", n)
	go capturePprofProfile(pprofCfg.BaseURL, pprofSeconds, pprofOutDir, tag)
}
