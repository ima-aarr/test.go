package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ==============================================================================
// [セクション1] 10万RPS対応: 高パフォーマンス・データ構造
// ==============================================================================

// TestConfig は、フロントエンド（Web UI）から受け取る負荷テストの実行パラメータを定義します。
// 10万RPSという超高負荷を前提とするため、KeepAliveなどは強制的に制御可能な設計としています。
type TestConfig struct {
	TargetURL   string `json:"target_url"`  // 攻撃対象の完全なURL
	Method      string `json:"method"`      // HTTPメソッド (GET, POST等)
	Concurrency int    `json:"concurrency"` // 同時実行数 (例: 10000)
	DurationSec int    `json:"duration"`    // 実行時間（秒）
	TimeoutSec  int    `json:"timeout"`     // リクエストタイムアウト（秒）
}

// ResultMetrics は、テストの実行結果を集約・保持するための構造体です。
// 10万RPS環境下で数万のGoroutineが同時に結果を書き込んでもロック競合による
// パフォーマンス低下（スロットリング）を起こさないよう、すべて atomic 操作前提で設計しています。
type ResultMetrics struct {
	// uint64を使用し、オーバーフローを防ぐとともにアトミック操作を可能にします
	TotalRequests uint64
	SuccessCount  uint64
	ErrorCount    uint64

	// ステータスコードごとのカウントを安全に記録するための sync.Map
	// キー: ステータスコード (int), 値: カウンタへのポインタ (*uint64)
	StatusCodes sync.Map

	// レイテンシ（応答時間）の記録
	// 10万RPS × 数十秒のテストでは数百万件のデータになるため、
	// Mutexによるロックは最小限にし、あらかじめキャパシティを確保したスライスを使用します。
	mu        sync.Mutex
	latencies []time.Duration
}

// NewResultMetrics は、パフォーマンスを最適化されたメトリクス構造体を初期化します。
// estimatedTotal (予想総リクエスト数) を基に、スライスのメモリを事前割り当て (Pre-allocation) し、
// テスト実行中の高コストなメモリアロケーションを防ぎます。
func NewResultMetrics(estimatedTotal int) *ResultMetrics {
	// 推定総リクエスト数に基づいて、スライスの初期容量（キャパシティ）を確保
	// 完全に一致しなくても、動的拡張の回数を激減させることでパフォーマンスが飛躍的に向上します
	return &ResultMetrics{
		TotalRequests: 0,
		SuccessCount:  0,
		ErrorCount:    0,
		latencies:     make([]time.Duration, 0, estimatedTotal),
	}
}

// Record は、各ワーカー（Goroutine）から単一のリクエスト結果を受け取り、スレッドセーフに記録します。
func (rm *ResultMetrics) Record(duration time.Duration, statusCode int, isError bool) {
	// 1. 総リクエスト数のアトミックなインクリメント
	atomic.AddUint64(&rm.TotalRequests, 1)

	// 2. 成功・エラーのアトミックな集計
	if isError {
		atomic.AddUint64(&rm.ErrorCount, 1)
	} else {
		// HTTP 2xx および 3xx を成功とみなす
		if statusCode >= 200 && statusCode < 400 {
			atomic.AddUint64(&rm.SuccessCount, 1)
		} else {
			atomic.AddUint64(&rm.ErrorCount, 1)
		}

		// ステータスコード分布の記録
		// LoadOrStore を使用して、既存のカウンタを取得するか新規作成します
		countPtr, _ := rm.StatusCodes.LoadOrStore(statusCode, new(uint64))
		atomic.AddUint64(countPtr.(*uint64), 1)
	}

	// 3. レイテンシデータの追加
	// ここは構造上 Mutex が必要ですが、処理を最小限（スライスへの append のみ）にとどめています
	rm.mu.Lock()
	rm.latencies = append(rm.latencies, duration)
	rm.mu.Unlock()
}

// TestReport は、テスト終了後にフロントエンド（UI）へ結果を返すためのJSON構造体です。
type TestReport struct {
	TotalRequests int               `json:"total_requests"`
	Success       int               `json:"success"`
	Errors        int               `json:"errors"`
	ThroughputRPS float64           `json:"throughput_rps"`
	MinLatency    string            `json:"min_latency"`
	MeanLatency   string            `json:"mean_latency"`
	P50Latency    string            `json:"p50_latency"`
	P90Latency    string            `json:"p90_latency"`
	P99Latency    string            `json:"p99_latency"`
	MaxLatency    string            `json:"max_latency"`
	StatusCodes   map[string]uint64 `json:"status_codes"`
	ErrorMsg      string            `json:"error_msg,omitempty"` // 致命的なエラーが発生した場合
}
// ==============================================================================
// [セクション2] 10万RPS対応: 超絶チューニング済みHTTPクライアントとワーカー
// ==============================================================================

// createOptimizedHTTPClient は、OSのエフェメラルポート枯渇を防ぎ、
// TCPコネクションを極限まで再利用するためのカスタムHTTPクライアントを生成します。
// 10万RPSを達成するための最重要コンポーネントです。
func createOptimizedHTTPClient(concurrency int, timeoutSec int) *http.Client {
	// タイムアウト値の計算
	timeout := time.Duration(timeoutSec) * time.Second
	if timeout == 0 {
		timeout = 10 * time.Second // デフォルトの安全値
	}

	// http.Transport はHTTP/TCP通信の低レイヤーを制御します
	transport := &http.Transport{
		// 【重要】MaxIdleConnsPerHost を並行数以上に設定します。
		// これを行わないと、コネクションプールが機能せず、TCPのTIME_WAITが大量発生してOSが死にます。
		MaxIdleConns:        concurrency * 2,
		MaxIdleConnsPerHost: concurrency * 2,
		MaxConnsPerHost:     concurrency * 2,

		// Keep-Alive を強制的に有効化し、ハンドシェイクのオーバーヘッドをゼロにします。
		DisableKeepAlives: false,

		// パフォーマンス向上のための各種タイムアウト設定
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,

		// どのような環境（自己署名証明書など）でもテストを止めないよう、TLS検証をスキップします。
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},

		// 高負荷時に100-Continueを待つオーバーヘッドを削減します
		ExpectContinueTimeout: 1 * time.Second,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		// 負荷テストの純粋なレスポンスタイムを測るため、リダイレクトは自動追従させずにエラーとして記録します
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return client
}

// executeWorker は、1つのGoroutineとして動作し、終了シグナルを受け取るまで
// ターゲットURLに対して限界までリクエストを連射し続けます。
func executeWorker(ctx context.Context, wg *sync.WaitGroup, client *http.Client, targetURL string, method string, metrics *ResultMetrics) {
	// ワーカー終了時にWaitGroupのカウントを減らす（これはGoroutineのライフサイクルにつき1回なのでdeferでOK）
	defer wg.Done()

	// 10万RPSを出すための最適化: ループの外でベースとなるリクエストオブジェクトを作成しておく。
	// ループ内で毎回 http.NewRequest を呼ぶと、極端な高負荷時にGC（ガベージコレクション）の対象となり、
	// メモリのアロケーションコストが無視できなくなるためです。
	baseReq, err := http.NewRequest(method, targetURL, nil)
	if err != nil {
		// リクエスト生成に失敗した場合（URLの構文エラーなど）は、このワーカーを即座に終了します。
		log.Printf("[Worker Error] リクエストの初期化に失敗しました: %v\n", err)
		return
	}

	// 無限ループでリクエストを送信し続ける（ctx.Done() で安全に抜け出します）
	for {
		select {
		case <-ctx.Done():
			// テスト時間が終了した、または強制中断された場合はループを抜ける
			return
		default:
			// ==================================================================
			// 限界突破の通信ループ（GC負荷を最小化する設計）
			// ==================================================================
			start := time.Now()

			// ベースリクエストをクローンし、コンテキスト（タイムアウト・キャンセル用）を付与します。
			// 完全な新規作成よりアロケーションを抑えられます。
			req := baseReq.Clone(ctx)

			// リクエスト実行
			resp, err := client.Do(req)
			duration := time.Since(start)

			if err != nil {
				// タイムアウト、ネットワーク切断などのエラー
				metrics.Record(duration, 0, true)
				continue
			}

			// 【重要】超高負荷対応のボディ破棄
			// レスポンスボディを最後まで読み切らないと、TCPコネクションがプールに返却されません。
			// io.Copy(io.Discard) を使い、データをメモリに確保せずブラックホールに捨てます。
			_, _ = io.Copy(io.Discard, resp.Body)
			
			// ループ内での defer resp.Body.Close() は、スコープを抜けるまで実行が遅延し
			// メモリリークやファイルディスクリプタの枯渇を招くため、必ず即座に手動で Close します。
			resp.Body.Close()

			// 成功または HTTPステータスエラー（404や500など）の記録
			metrics.Record(duration, resp.StatusCode, false)
		}
	}
}
// ==============================================================================
// [セクション3] 10万RPS対応: オーケストレーターと高速集計ロジック
// ==============================================================================

// formatDuration は time.Duration をフロントエンドで表示しやすいミリ秒単位の文字列に変換します。
// 例: 1.23ms
func formatDuration(d time.Duration) string {
	return fmt.Sprintf("%.2fms", float64(d.Microseconds())/1000.0)
}

// generateReport は、収集されたメトリクスと実際の実行時間から、フロントエンドへ返すJSONレポートを生成します。
func generateReport(metrics *ResultMetrics, actualDuration time.Duration) *TestReport {
	report := &TestReport{
		TotalRequests: int(atomic.LoadUint64(&metrics.TotalRequests)),
		Success:       int(atomic.LoadUint64(&metrics.SuccessCount)),
		Errors:        int(atomic.LoadUint64(&metrics.ErrorCount)),
		StatusCodes:   make(map[string]uint64),
	}

	// 1. 実際のスループット (RPS: Requests Per Second) の計算
	// 実行時間が0になるゼロ除算エラーを防ぐための安全策
	durationSec := actualDuration.Seconds()
	if durationSec <= 0 {
		durationSec = 0.0001
	}
	report.ThroughputRPS = float64(report.TotalRequests) / durationSec

	// 2. HTTPステータスコード分布の集計
	metrics.StatusCodes.Range(func(key, value interface{}) bool {
		statusCode := key.(int)
		count := atomic.LoadUint64(value.(*uint64))
		
		// ステータスコードが0の場合は、タイムアウト等のネットワークエラー
		if statusCode == 0 {
			report.StatusCodes["NetworkError"] = count
		} else {
			report.StatusCodes[fmt.Sprintf("%d", statusCode)] = count
		}
		return true
	})

	// 3. レイテンシ（応答時間）のパーセンタイルと統計計算
	metrics.mu.Lock()
	// 高速化のため、ここでスライスの参照だけを取得し、以後はロック不要で処理します
	latencies := metrics.latencies
	metrics.mu.Unlock()

	totalLatencies := len(latencies)

	if totalLatencies > 0 {
		// スライスを昇順にソート（数百万件でもGoの標準ソートは非常に高速です）
		sort.Slice(latencies, func(i, j int) bool {
			return latencies[i] < latencies[j]
		})

		// 最小値と最大値
		report.MinLatency = formatDuration(latencies[0])
		report.MaxLatency = formatDuration(latencies[totalLatencies-1])

		// 平均値の計算（オーバーフローを防ぐため、マイクロ秒単位で合算して平均を取ります）
		var sumMicro int64
		for _, l := range latencies {
			sumMicro += l.Microseconds()
		}
		meanMicro := float64(sumMicro) / float64(totalLatencies)
		report.MeanLatency = fmt.Sprintf("%.2fms", meanMicro/1000.0)

		// パーセンタイル（p50, p90, p99）のインデックス計算
		p50Idx := int(float64(totalLatencies) * 0.50)
		p90Idx := int(float64(totalLatencies) * 0.90)
		p99Idx := int(float64(totalLatencies) * 0.99)

		// インデックスが配列の範囲を超えないよう安全装置（フェイルセーフ）を設ける
		if p50Idx >= totalLatencies { p50Idx = totalLatencies - 1 }
		if p90Idx >= totalLatencies { p90Idx = totalLatencies - 1 }
		if p99Idx >= totalLatencies { p99Idx = totalLatencies - 1 }

		report.P50Latency = formatDuration(latencies[p50Idx])
		report.P90Latency = formatDuration(latencies[p90Idx])
		report.P99Latency = formatDuration(latencies[p99Idx])
	} else {
		// リクエストが1件も成功・記録されなかった場合のフォールバック
		zero := "0.00ms"
		report.MinLatency, report.MeanLatency, report.P50Latency = zero, zero, zero
		report.P90Latency, report.P99Latency, report.MaxLatency = zero, zero, zero
	}

	return report
}

// runLoadTest はフロントエンドからの設定を受け取り、負荷テスト全体を指揮（オーケストレーション）します。
func runLoadTest(cfg *TestConfig) *TestReport {
	// メモリ事前割り当てのための推定総リクエスト数を計算
	// (並行数 * 予想RPS * 秒数) で大まかなキャパシティを算出します
	estimatedTotal := cfg.Concurrency * 100 * cfg.DurationSec
	if estimatedTotal <= 0 {
		estimatedTotal = 10000 // フォールバック値
	}

	// ゼロアロケーションを目指すメトリクス構造体の初期化
	metrics := NewResultMetrics(estimatedTotal)

	// OSリソースを極限まで使い倒す最適化済みHTTPクライアントの生成
	client := createOptimizedHTTPClient(cfg.Concurrency, cfg.TimeoutSec)

	// コンテキストによる実行時間の厳格な管理
	// 指定された秒数が経過すると、全ワーカーへ一斉にキャンセルシグナルが送信されます
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.DurationSec)*time.Second)
	defer cancel()

	var wg sync.WaitGroup

	log.Printf("[Orchestrator] テストを開始します: %s, 並行数: %d, 実行時間: %d秒\n", cfg.TargetURL, cfg.Concurrency, cfg.DurationSec)

	// 正確なスループット計算のための開始時間記録
	startTime := time.Now()

	// 限界突破のワーカー一斉起動（GoのGoroutineは非常に軽量なため、数万個でも瞬時に起動します）
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go executeWorker(ctx, &wg, client, cfg.TargetURL, cfg.Method, metrics)
	}

	// すべてのワーカーが終了（またはタイムアウトでキャンセル）するまでブロックして待機
	wg.Wait()

	// 実際の実行時間を計測（コンテキストによる停止処理にかかったわずかな時間も含みます）
	actualDuration := time.Since(startTime)
	log.Printf("[Orchestrator] テスト完了。実際の実行時間: %v. 結果を集計中...\n", actualDuration)

	// 収集したメトリクスから最終レポートを生成して返す
	return generateReport(metrics, actualDuration)
}
// ==============================================================================
// [セクション4] 10万RPS対応: APIサーバー基盤（CORS突破・JSONハンドリング）
// ==============================================================================

// enableCORS は、APIエンドポイントに対するすべてのCORS制限を解除します。
// これにより、将来的にどのドメインのブラウザアプリからでもこのエンジンを操作可能になります。
func enableCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
}

// handleAPI は、フロントエンド（Web UI）からの負荷テスト実行リクエストを受け付けるエンドポイントです。
func handleAPI(w http.ResponseWriter, r *http.Request) {
	// 1. CORS制限の解除設定
	enableCORS(w)

	// ブラウザからのプリフライトリクエスト (OPTIONS) には 200 OK を返して即終了
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	// 負荷テストの実行指示は POST メソッドのみ受け付けます
	if r.Method != http.MethodPost {
		http.Error(w, `{"error_msg": "POSTメソッドのみ許可されています"}`, http.StatusMethodNotAllowed)
		return
	}

	// 2. フロントエンドからのJSONペイロードの読み込みと解析
	var cfg TestConfig
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("[API Error] リクエストボディの読み込みに失敗しました: %v\n", err)
		http.Error(w, `{"error_msg": "無効なリクエストボディです"}`, http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if err := json.Unmarshal(body, &cfg); err != nil {
		log.Printf("[API Error] JSONの解析に失敗しました: %v\n", err)
		http.Error(w, `{"error_msg": "JSONフォーマットが正しくありません"}`, http.StatusBadRequest)
		return
	}

	// 3. 入力値の厳格なバリデーションと安全なデフォルト値へのフォールバック
	if cfg.TargetURL == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(TestReport{ErrorMsg: "ターゲットURLが指定されていません"})
		return
	}
	if cfg.Method == "" {
		cfg.Method = "GET"
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 100 // 安全なデフォルト値
	}
	if cfg.DurationSec <= 0 {
		cfg.DurationSec = 10 // 安全なデフォルト値
	}
	if cfg.TimeoutSec <= 0 {
		cfg.TimeoutSec = 5 // デフォルトのタイムアウト
	}

	log.Printf("[API] 負荷テストのリクエストを受信しました。ターゲット: %s", cfg.TargetURL)

	// 4. 負荷テストエンジンの起動（オーケストレーターの呼び出し）
	// ここでメインスレッドはテスト完了までブロックされます
	report := runLoadTest(&cfg)

	// 5. テスト結果（レポート）をJSONとしてフロントエンドへ返却
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	
	if err := json.NewEncoder(w).Encode(report); err != nil {
		log.Printf("[API Error] レポートのJSONエンコードに失敗しました: %v\n", err)
	}
}
// ==============================================================================
// [セクション5] 10万RPS対応: フルスタックWeb UI (HTML/CSS/JS 埋め込み)
// ==============================================================================

// indexHTML は、ブラウザに配信されるフロントエンドの完全なソースコードです。
// この文字列がGoのバイナリに直接埋め込まれるため、デプロイ時に外部ファイルは一切不要になります。
const indexHTML = `<!DOCTYPE html>
<html lang="ja">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>UltraLoad - Professional Load Tester</title>
    <style>
        body {
            font-family: 'Segoe UI', system-ui, -apple-system, sans-serif;
            background-color: #f3f4f6;
            color: #1f2937;
            margin: 0;
            padding: 2rem;
            display: flex;
            justify-content: center;
        }
        .container {
            background: #ffffff;
            width: 100%;
            max-width: 800px;
            padding: 2.5rem;
            border-radius: 12px;
            box-shadow: 0 10px 25px rgba(0,0,0,0.05);
        }
        h1 { margin-top: 0; color: #2563eb; font-size: 2rem; border-bottom: 2px solid #e5e7eb; padding-bottom: 1rem; }
        .grid { display: grid; grid-template-columns: 1fr 1fr; gap: 1.5rem; margin-bottom: 1.5rem; }
        .form-group { display: flex; flex-direction: column; }
        .form-group.full { grid-column: span 2; }
        label { font-weight: 600; margin-bottom: 0.5rem; font-size: 0.95rem; color: #4b5563; }
        input, select {
            padding: 0.75rem;
            border: 1px solid #d1d5db;
            border-radius: 6px;
            font-size: 1rem;
            transition: border-color 0.2s;
        }
        input:focus, select:focus { outline: none; border-color: #2563eb; box-shadow: 0 0 0 3px rgba(37,99,235,0.1); }
        button {
            background-color: #2563eb; color: white; border: none; padding: 1rem;
            width: 100%; border-radius: 6px; font-size: 1.1rem; font-weight: bold;
            cursor: pointer; transition: background-color 0.2s;
        }
        button:hover { background-color: #1d4ed8; }
        button:disabled { background-color: #9ca3af; cursor: not-allowed; }
        
        #results { margin-top: 2rem; display: none; }
        .result-box {
            background-color: #1f2937; color: #10b981; padding: 1.5rem;
            border-radius: 8px; font-family: 'Courier New', monospace;
            white-space: pre-wrap; word-break: break-all; font-size: 0.95rem;
        }
        .status-error { color: #ef4444; }
        .status-loading { color: #f59e0b; }
    </style>
</head>
<body>

<div class="container">
    <h1>🚀 UltraLoad Engine</h1>
    <p style="color: #6b7280; margin-bottom: 2rem;">
        フロントエンドからGo言語のコアエンジンへ直接指示を出し、10万RPS規模の極限負荷テストを実行します。
    </p>

    <div class="grid">
        <div class="form-group full">
            <label for="url">ターゲットURL (必須)</label>
            <input type="text" id="url" placeholder="https://example.com/api" required>
        </div>
        
        <div class="form-group">
            <label for="method">HTTP メソッド</label>
            <select id="method">
                <option value="GET">GET</option>
                <option value="POST">POST</option>
                <option value="PUT">PUT</option>
                <option value="DELETE">DELETE</option>
            </select>
        </div>
        
        <div class="form-group">
            <label for="concurrency">並行ワーカー数 (1 - 100000)</label>
            <input type="number" id="concurrency" value="1000" min="1">
        </div>
        
        <div class="form-group">
            <label for="duration">実行時間 (秒)</label>
            <input type="number" id="duration" value="10" min="1">
        </div>

        <div class="form-group">
            <label for="timeout">タイムアウト (秒)</label>
            <input type="number" id="timeout" value="5" min="1">
        </div>
    </div>

    <button id="runBtn" onclick="startTest()">🔥 限界負荷テストを開始</button>

    <div id="results">
        <h2 style="font-size: 1.5rem; color: #374151;">📊 実行レポート</h2>
        <div id="output" class="result-box"></div>
    </div>
</div>

<script>
    async function startTest() {
        const btn = document.getElementById('runBtn');
        const resultsDiv = document.getElementById('results');
        const output = document.getElementById('output');
        
        const url = document.getElementById('url').value;
        if (!url) {
            alert("ターゲットURLを入力してください。");
            return;
        }

        // UIを待機状態に変更
        btn.disabled = true;
        btn.innerText = "⏳ テスト実行中 (エンジン稼働中)...";
        resultsDiv.style.display = "block";
        output.className = "result-box status-loading";
        output.innerText = "[Orchestrator] バックエンドエンジンにテストを指示しました...\nターゲット: " + url + "\n(指定された実行時間が経過するまでお待ちください)";

        const payload = {
            target_url: url,
            method: document.getElementById('method').value,
            concurrency: parseInt(document.getElementById('concurrency').value, 10),
            duration: parseInt(document.getElementById('duration').value, 10),
            timeout: parseInt(document.getElementById('timeout').value, 10)
        };

        try {
            // Go言語のAPIハンドラーへPOSTリクエストを送信
            const response = await fetch('/api/run', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(payload)
            });

            const data = await response.json();

            if (!response.ok || data.error_msg) {
                output.className = "result-box status-error";
                output.innerText = "[Error] テストに失敗しました:\n" + (data.error_msg || "Unknown Server Error");
                return;
            }

            // 結果のフォーマットと表示
            output.className = "result-box";
            let reportText = "==================================================\n";
            reportText += "✅ テスト完了 (Go Engine API)\n";
            reportText += "==================================================\n\n";
            reportText += "[基本統計]\n";
            reportText += "総リクエスト数 : " + data.total_requests.toLocaleString() + "\n";
            reportText += "成功 (2xx/3xx) : " + data.success.toLocaleString() + "\n";
            reportText += "エラー (4xx/5xx): " + data.errors.toLocaleString() + "\n";
            reportText += "スループット   : " + data.throughput_rps.toFixed(2) + " RPS (リクエスト/秒)\n\n";
            
            reportText += "[レイテンシ (応答時間)]\n";
            reportText += "最小 (Min)   : " + data.min_latency + "\n";
            reportText += "平均 (Mean)  : " + data.mean_latency + "\n";
            reportText += "中央値 (p50) : " + data.p50_latency + "\n";
            reportText += "p90          : " + data.p90_latency + "\n";
            reportText += "p99          : " + data.p99_latency + "\n";
            reportText += "最大 (Max)   : " + data.max_latency + "\n\n";

            reportText += "[ステータスコード分布]\n";
            for (const [code, count] of Object.entries(data.status_codes)) {
                reportText += "HTTP " + code + " : " + count.toLocaleString() + " 件\n";
            }
            reportText += "==================================================";

            output.innerText = reportText;

        } catch (error) {
            output.className = "result-box status-error";
            output.innerText = "[Fatal Error] バックエンドとの通信に失敗しました。\n" + error.message;
        } finally {
            // UIの状態をリセット
            btn.disabled = false;
            btn.innerText = "🔥 限界負荷テストを開始";
        }
    }
</script>
</body>
</html>`
// ==============================================================================
// [セクション6] 10万RPS対応: UIルーターと安全な終了処理 (Graceful Shutdown)
// ==============================================================================

// handleUI は、埋め込まれたフロントエンド (HTML/CSS/JS) をブラウザに対して配信します。
// 物理的なファイルI/Oが発生しないため、超高速かつ外部ファイルに依存しません。
func handleUI(w http.ResponseWriter, r *http.Request) {
	// ルートパス ("/") 以外のアクセスは 404 Not Found として処理します
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	// レスポンスヘッダーに文字コードとコンテンツタイプを設定
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	// 文字列定数として埋め込まれたHTMLをバイト配列に変換して直接書き込みます
	if _, err := w.Write([]byte(indexHTML)); err != nil {
		log.Printf("[UI Error] HTMLの配信中にエラーが発生しました: %v\n", err)
	}
}

// setupGracefulShutdown は、OSからの割り込みシグナル（Ctrl+Cなど）を監視し、
// 通信中のリクエストが強制切断されるのを防ぐためのシャットダウンプロセスを管理します。
func setupGracefulShutdown(server *http.Server) {
	// OSシグナルを受信するためのバッファ付きチャネルを作成
	quit := make(chan os.Signal, 1)
	
	// SIGINT (Ctrl+Cによる割り込み) と SIGTERM (システムによる終了要求) を監視対象に設定
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// メインスレッドをブロックしないよう、専用のGoroutineでシグナルを待機します
	go func() {
		// シグナルが受信されるまでここで待機（ブロック）
		sig := <-quit
		log.Printf("\n[System] シグナル (%v) を受信しました。サーバーを安全に停止します...\n", sig)

		// 現在処理中のリクエスト（最大10万RPSの負荷テストなど）が完了するのを待つための猶予時間（15秒）を設定
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		// サーバーの新規リクエスト受付を停止し、処理中のコネクションが完了するまで待機（Graceful Shutdown）
		if err := server.Shutdown(ctx); err != nil {
			log.Fatalf("[System Error] サーバーのシャットダウン中に致命的なエラーが発生しました: %v\n", err)
		}

		log.Println("[System] サーバープロセスが正常に終了しました。")
	}()
}
// ==============================================================================
// [セクション7] メイン関数 (Entry Point) とサーバー起動
// ==============================================================================

// main はこのプログラムのエントリーポイントです。
// ルーティングの設定、サーバーの構成、および起動処理を一元管理します。
func main() {
	// 1. ルーティングの設定 (マルチプレクサの作成)
	// http.DefaultServeMux を避けることで、意図しないエンドポイントの公開を防ぎます (セキュリティ対策)
	mux := http.NewServeMux()

	// 埋め込みWeb UI（ダッシュボード）の配信ルート
	mux.HandleFunc("/", handleUI)

	// フロントエンドからの負荷テスト実行要求を受け付けるAPIルート
	mux.HandleFunc("/api/run", handleAPI)

	// 2. HTTPサーバーの設定
	// タイムアウトを適切に設定し、スローロリス攻撃(Slowloris)などのコネクション枯渇攻撃からシステムを守ります
	server := &http.Server{
		Addr:         ":8080",           // 待ち受けるポート番号
		Handler:      mux,               // カスタムマルチプレクサを指定
		ReadTimeout:  10 * time.Second,  // リクエストヘッダーの読み込みタイムアウト
		// WriteTimeoutは、最長のテスト実行時間（DurationSec）を考慮して十分に長くするか、
		// 完全にストリーミング処理する場合はタイムアウトを外す設計もありますが、今回は安全のため設定します。
		// ※超長時間のテストを行う場合は、ここを適宜伸ばしてください。
		WriteTimeout: 3600 * time.Second, 
		IdleTimeout:  120 * time.Second, // キープアライブ通信時の待機タイムアウト
	}

	// 3. Graceful Shutdown（安全な終了処理）のセットアップ
	// サーバーインスタンスを渡し、OSシグナル（Ctrl+C等）を監視するバックグラウンド処理を開始します
	setupGracefulShutdown(server)

	// 4. サーバーの起動と運用案内
	log.Println("======================================================")
	log.Println("🚀 UltraLoad Engine - Professional Load Tester started")
	log.Println("======================================================")
	log.Println("[INFO] ブラウザで以下のURLにアクセスしてUIを開いてください:")
	log.Println("[INFO] http://localhost:8080")
	log.Println("======================================================")

	// ListenAndServe はサーバーが停止するまでメインスレッドをブロックし続けます
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		// ErrServerClosed は Graceful Shutdown による正常な停止を示すため、
		// それ以外の予期せぬエラー（ポートが既に使用されている等）のみを Fatal として扱います
		log.Fatalf("[System Fatal] サーバーの起動または実行中に致命的なエラーが発生しました: %v\n", err)
	}
}
