package main

import (
	"crypto/tls"
	"flag"
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
// 1. データ構造の定義 (Data Structures)
// ==============================================================================

// Config は負荷テストの実行条件を保持する構造体です。
// プロダクションレベルでの拡張性を考慮し、細かなタイムアウト設定も含めています。
type Config struct {
	URL         string        // ターゲットURL
	Method      string        // HTTPメソッド (GET, POST等)
	Concurrency int           // 並行して実行するワーカー（Goroutine）の数
	Duration    time.Duration // テストを実行する総時間
	Timeout     time.Duration // 各リクエストのタイムアウト時間
	KeepAlive   bool          // HTTP Keep-Aliveを有効にするかどうか（TCP接続の再利用）
}

// Result は1つのHTTPリクエストの実行結果を保持します。
type Result struct {
	Duration time.Duration // リクエスト開始から完了までのレイテンシ
	Error    error         // 発生したエラー（成功時はnil）
	Status   int           // HTTPステータスコード
}

// Metrics は負荷テスト全体の統計情報をスレッドセーフに保持・集計するための構造体です。
// 高負荷時のロック競合を防ぐため、単純なカウントには sync/atomic を使用します。
type Metrics struct {
	TotalRequests uint64          // 送信した総リクエスト数
	Success       uint64          // 成功したリクエスト数 (2xx, 3xx)
	Errors        uint64          // 失敗したリクエスト数 (ネットワークエラー含む)
	
	mu            sync.Mutex      // latencies スライスへの書き込みを保護するミューテックス
	latencies     []time.Duration // 全リクエストのレイテンシを記録するスライス（パーセンタイル計算用）
	
	statusCodes   sync.Map        // ステータスコードの分布を記録する並行処理対応マップ
}

// AddResult は各ワーカーから報告された結果を Metrics に安全に記録します。
func (m *Metrics) AddResult(res Result) {
	// 1. 総リクエスト数のインクリメント (アトミック操作でロックフリー)
	atomic.AddUint64(&m.TotalRequests, 1)

	// 2. 成功/失敗のカウントとステータスコードの記録
	if res.Error != nil {
		atomic.AddUint64(&m.Errors, 1)
	} else {
		if res.Status >= 200 && res.Status < 400 {
			atomic.AddUint64(&m.Success, 1)
		} else {
			atomic.AddUint64(&m.Errors, 1)
		}
		
		// ステータスコードの出現回数をカウント
		count, _ := m.statusCodes.LoadOrStore(res.Status, new(uint64))
		atomic.AddUint64(count.(*uint64), 1)
	}

	// 3. レイテンシの記録 (スライスへのアペンドはミューテックスで保護)
	m.mu.Lock()
	m.latencies = append(m.latencies, res.Duration)
	m.mu.Unlock()
}

// ==============================================================================
// 2. CLI引数の解析と初期化 (Initialization)
// ==============================================================================

// parseConfig はコマンドライン引数を解析し、Config構造体を生成します。
func parseConfig() *Config {
	cfg := &Config{}
	
	flag.StringVar(&cfg.URL, "u", "", "ターゲットURL (例: http://example.com)")
	flag.StringVar(&cfg.Method, "m", "GET", "HTTPメソッド")
	flag.IntVar(&cfg.Concurrency, "c", 50, "並行実行するワーカー数 (デフォルト: 50)")
	flag.DurationVar(&cfg.Duration, "d", 10*time.Second, "実行時間 (例: 10s, 1m)")
	flag.DurationVar(&cfg.Timeout, "t", 5*time.Second, "各リクエストのタイムアウト (例: 5s)")
	flag.BoolVar(&cfg.KeepAlive, "k", true, "HTTP Keep-Aliveを有効にする (デフォルト: true)")

	flag.Parse()

	// 必須パラメータのバリデーション
	if cfg.URL == "" {
		fmt.Println("エラー: ターゲットURL (-u) は必須です。")
		flag.Usage()
		os.Exit(1)
	}

	if cfg.Concurrency <= 0 {
		log.Fatalf("エラー: 並行数 (-c) は1以上を指定してください。")
	}

	return cfg
}
// ==============================================================================
// 3. HTTPクライアントの最適化 (Custom HTTP Client)
// ==============================================================================

// createHTTPClient は、高負荷な環境でも OS のリソース（特にエフェメラルポート）を
// 枯渇させないようにチューニングされたカスタム HTTP クライアントを生成します。
// TIME_WAIT 状態のソケットが大量発生するのを防ぐため、コネクションプーリングを極限まで最適化しています。
func createHTTPClient(cfg *Config) *http.Client {
	// デフォルトの HTTP トランスポートをベースに、負荷テスト用のチューニングを施します。
	transport := &http.Transport{
		// MaxIdleConns は、すべてのホストに対するアイドル状態のコネクションの最大数です。
		// 大規模なテストに備えて十分に大きな値を設定します。
		MaxIdleConns: 10000,
		
		// MaxIdleConnsPerHost は、1つのホストに対するアイドル状態のコネクションの最大数です。
		// 【重要】ここを並行数（Concurrency）以上に設定しないと、コネクションが再利用されず、
		// 毎回 TCP ハンドシェイクが発生してしまい、パフォーマンスが著しく低下します。
		MaxIdleConnsPerHost: cfg.Concurrency,
		
		// TLS の証明書検証をスキップします。
		// （ステージング環境や自己署名証明書でのテストをエラーなく実行可能にするため）
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
		
		// Keep-Alive を無効にするかどうかの設定です。
		// 意図的に毎回コネクションを切断するテストを行いたい場合以外は false (有効) にします。
		DisableKeepAlives: !cfg.KeepAlive,
		
		// アイドル状態のコネクションを維持する最大時間です。
		IdleConnTimeout: 90 * time.Second,
		
		// TLS ハンドシェイクのタイムアウト時間です。
		TLSHandshakeTimeout: 10 * time.Second,
		
		// レスポンスヘッダの読み込みタイムアウト時間です。
		ResponseHeaderTimeout: cfg.Timeout,
	}

	// HTTP クライアント本体の生成。タイムアウトはユーザーの設定値を使用します。
	client := &http.Client{
		Transport: transport,
		Timeout:   cfg.Timeout,
		// リダイレクトを自動で追従させない設定です。
		// （負荷テストでは意図しない外部ドメインへのリクエストを防ぎ、純粋な対象URLの応答を測るため）
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return client
}

// ==============================================================================
// 4. 負荷生成ワーカー (Load Generator Worker)
// ==============================================================================

// runWorker は、単一の Goroutine として動作し、終了シグナルを受け取るまで
// ターゲット URL に対して継続的に HTTP リクエストを送信し続けます。
func runWorker(ctx context.Context, wg *sync.WaitGroup, client *http.Client, cfg *Config, metrics *Metrics) {
	// ワーカー終了時に WaitGroup のカウントを減らします。
	defer wg.Done()

	for {
		select {
		case <-ctx.Done():
			// コンテキストがキャンセルされた（指定したテスト時間が終了した）場合は、
			// ループを抜けてワーカーを安全に停止します。
			return
		default:
			// リクエスト開始前の時間を記録
			start := time.Now()
			
			// リクエストオブジェクトの生成
			// コンテキストを渡すことで、タイムアウト時に即座にリクエストを中断できるようにします。
			req, err := http.NewRequestWithContext(ctx, cfg.Method, cfg.URL, nil)
			if err != nil {
				// リクエスト生成エラーの記録
				metrics.AddResult(Result{
					Duration: time.Since(start),
					Error:    err,
					Status:   0,
				})
				continue
			}

			// リクエストの送信
			resp, err := client.Do(req)
			duration := time.Since(start)

			if err != nil {
				// タイムアウトやネットワークエラーの記録
				metrics.AddResult(Result{
					Duration: duration,
					Error:    err,
					Status:   0,
				})
				continue
			}

			// 【重要】レスポンスボディの読み捨て
			// コネクションをプールに返却して再利用するためには、ボディを最後まで読み切る必要があります。
			// io.Copy(io.Discard, ...) を使うことで、メモリにデータを保持せずに最速で読み捨てます。
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			// 成功または HTTP レベルのエラー（404, 500など）の記録
			metrics.AddResult(Result{
				Duration: duration,
				Error:    nil,
				Status:   resp.StatusCode,
			})
		}
	}
}
// ==============================================================================
// 5. テスト実行エンジン (Load Test Orchestrator / Runner)
// ==============================================================================

// runLoadTest は設定に基づき負荷テスト全体を指揮（オーケストレーション）します。
// 指定された並行数（Concurrency）分のワーカーを起動し、テスト完了または中断シグナルを待ち受けます。
func runLoadTest(cfg *Config) *Metrics {
	// メトリクス集計用の構造体を初期化
	metrics := &Metrics{}

	// 最適化済みの高パフォーマンスHTTPクライアントを生成
	client := createHTTPClient(cfg)

	// 実行時間を制御するための Context を作成
	// ユーザーが指定した Duration (例: 10秒) が経過すると、自動的に Done() チャネルが閉じられ、
	// 全てのワーカーがそれを検知して一斉に安全に停止します。
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Duration)
	
	// 関数の終了時（テスト完了時、またはシグナル受信時）に確実にリソースを解放します。
	defer cancel()

	// ==========================================================================
	// Graceful Shutdown (安全な中断) のためのシグナルハンドリング
	// ==========================================================================
	// OSからの割り込みシグナル（Ctrl+C など）を監視するチャネルを作成します。
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	
	// シグナル監視用の別 Goroutine を起動します。
	// テスト実行中に強制終了された場合でも、Context をキャンセルすることで
	// ワーカーを安全に停止させ、そこまでの結果を集計して出力できるようにします。
	go func() {
		<-sigChan
		fmt.Println("\n[!] 割り込みシグナルを受信しました。テストを安全に中断し、結果を集計します...")
		cancel() // Context をキャンセルし、全ワーカーの通信ループを終了させる
	}()

	// ==========================================================================
	// ワーカーの起動と待機 (Concurrency Management)
	// ==========================================================================
	// 全ワーカーの完了を待ち合わせるための WaitGroup
	var wg sync.WaitGroup

	// ユーザーへの実行開始アナウンス
	fmt.Printf("🎯 ターゲットURL: %s\n", cfg.URL)
	fmt.Printf("🚀 並行ワーカー数: %d\n", cfg.Concurrency)
	fmt.Printf("⏱️  予定実行時間: %v\n", cfg.Duration)
	fmt.Printf("🔌 Keep-Alive:    %v\n", cfg.KeepAlive)
	fmt.Println("--------------------------------------------------")
	fmt.Println("🔥 負荷テストを実行中です。しばらくお待ちください...")

	// 実際のテスト開始時間を記録（後で正確な RPS を計算するため）
	startTime := time.Now()

	// 設定された並行数（Concurrency）だけ、一斉にワーカー（Goroutine）を起動します。
	// ここが負荷生成のコアとなるループです。
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		// 各 Goroutine は同じ Context, WaitGroup, Client, Metrics への参照を共有します。
		// これにより、ロック競合を最小限に抑えつつ超高並行処理を実現します。
		go runWorker(ctx, &wg, client, cfg, metrics)
	}

	// 全てのワーカーが終了（各ワーカー内で wg.Done() が呼ばれる）するまでメインスレッドをブロックして待機します。
	wg.Wait()

	// 実際のテスト実行時間を計算（途中で中断された場合は予定実行時間より短くなります）
	actualDuration := time.Since(startTime)
	fmt.Printf("✅ テスト完了 (実際の実行時間: %v)\n", actualDuration)

	// 計測結果の入った Metrics をレポーターへ返す
	return metrics
}
// ==============================================================================
// 6. レポート生成エンジン (Result Aggregator & Reporter)
// ==============================================================================

// printReport は集計されたメトリクスを受け取り、統計情報を計算してコンソールに出力します。
// 実行時間（actualDuration）を用いて正確なスループット（RPS）を算出します。
func printReport(metrics *Metrics, actualDuration time.Duration) {
	fmt.Println("\n==================================================")
	fmt.Println("📊 負荷テスト 実行結果レポート")
	fmt.Println("==================================================")

	// 総リクエスト数が0の場合は、エラーメッセージを出力して終了します。
	if metrics.TotalRequests == 0 {
		fmt.Println("[!] リクエストが1件も送信されませんでした。ネットワーク接続やURLを確認してください。")
		return
	}

	// 1. スループット（RPS: Requests Per Second）の計算
	// actualDuration.Seconds() が 0 にならないように安全策を取ります。
	durationSec := actualDuration.Seconds()
	if durationSec <= 0 {
		durationSec = 0.0001
	}
	rps := float64(metrics.TotalRequests) / durationSec

	// 基本的なリクエスト統計を出力
	fmt.Printf("総リクエスト数: %d\n", metrics.TotalRequests)
	fmt.Printf("成功リクエスト: %d\n", metrics.Success)
	fmt.Printf("失敗リクエスト: %d\n", metrics.Errors)
	fmt.Printf("スループット:   %.2f RPS (リクエスト/秒)\n", rps)

	// 2. レイテンシ（応答時間）のパーセンタイル計算
	// レイテンシのスライスを昇順にソートします。
	// データ量が多い場合でも Go の標準 sort は非常に高速です。
	sort.Slice(metrics.latencies, func(i, j int) bool {
		return metrics.latencies[i] < metrics.latencies[j]
	})

	totalLatencies := len(metrics.latencies)
	
	if totalLatencies > 0 {
		// 最小値と最大値
		min := metrics.latencies[0]
		max := metrics.latencies[totalLatencies-1]

		// 平均値の計算
		var sum time.Duration
		for _, l := range metrics.latencies {
			sum += l
		}
		mean := time.Duration(int64(sum) / int64(totalLatencies))

		// パーセンタイルのインデックス計算
		p50Index := int(float64(totalLatencies) * 0.50)
		p90Index := int(float64(totalLatencies) * 0.90)
		p99Index := int(float64(totalLatencies) * 0.99)

		// インデックスが配列の範囲を超えないように補正（フェイルセーフ）
		if p50Index >= totalLatencies { p50Index = totalLatencies - 1 }
		if p90Index >= totalLatencies { p90Index = totalLatencies - 1 }
		if p99Index >= totalLatencies { p99Index = totalLatencies - 1 }

		p50 := metrics.latencies[p50Index]
		p90 := metrics.latencies[p90Index]
		p99 := metrics.latencies[p99Index]

		fmt.Println("\n⏱️  レイテンシ (応答時間) 統計:")
		fmt.Printf("  最小 (Min):    %v\n", min)
		fmt.Printf("  平均 (Mean):   %v\n", mean)
		fmt.Printf("  中央値 (p50):  %v\n", p50)
		fmt.Printf("  90パーセンタイル (p90): %v\n", p90)
		fmt.Printf("  99パーセンタイル (p99): %v\n", p99)
		fmt.Printf("  最大 (Max):    %v\n", max)
	}

	// 3. HTTP ステータスコードの分布を出力
	fmt.Println("\n📈 HTTP ステータスコード分布:")
	hasStatus := false
	
	// sync.Map に記録されたステータスコードを順番に読み出して出力します。
	metrics.statusCodes.Range(func(key, value interface{}) bool {
		hasStatus = true
		statusCode := key.(int)
		count := atomic.LoadUint64(value.(*uint64)) // アトミックに読み出し
		
		// ステータスコードが0の場合は、ネットワークエラー（タイムアウトや接続拒否など）を示します
		if statusCode == 0 {
			fmt.Printf("  [Network Error / Timeout] : %d 件\n", count)
		} else {
			fmt.Printf("  [%d] : %d 件\n", statusCode, count)
		}
		// true を返すことでイテレーションを継続します
		return true
	})

	if !hasStatus {
		fmt.Println("  (記録されたステータスコードはありません)")
	}
	fmt.Println("==================================================")
}
// ==============================================================================
// 7. メイン関数 (Entry Point)
// ==============================================================================

// main はこの負荷テストツールのエントリーポイントです。
// 設定の解析、テストの実行、および結果の出力を順次呼び出します。
func main() {
	// 1. コマンドライン引数の解析と設定のロード
	// ユーザーが指定したURLや並行数、実行時間を Config 構造体として取得します。
	cfg := parseConfig()

	// 2. 負荷テストの実行（オーケストレーション）と、開始・終了時間の計測
	// 実際のテストにかかった時間を正確に測るため、開始直前の時間を記録します。
	startTime := time.Now()
	
	// オーケストレータを呼び出し、数千・数万のGoroutineを起動して負荷をかけます。
	// 戻り値として、全ワーカーから集約されたスレッドセーフな Metrics を受け取ります。
	metrics := runLoadTest(cfg)
	
	// 実際の実行時間を算出します（途中でCtrl+C等で中断された場合も正確に計算されます）。
	actualDuration := time.Since(startTime)

	// 3. テスト結果の集計とレポート出力
	// 収集したメトリクスと実際の実行時間をレポーターに渡し、コンソールに統計を出力します。
	printReport(metrics, actualDuration)
}
