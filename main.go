package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/h264reader"
)

const (
	pionServerIP    = "127.0.0.2"
	pionServerPort  = "5004"
	testDurationSec = 45 // fallback default; overridden by TEST_DURATION env var

	defaultVideoFile = "media/bbb_4mbps.h264"
	defaultVideoFPS  = 30
)

var signalingAddr = func() string {
	if v := os.Getenv("SIGNAL_ADDR"); v != "" {
		return v
	}
	return "127.0.0.1:8080"
}()

// FrameRecord 记录服务端收到的每一帧信息
type FrameRecord struct {
	RecvTimeUs int64  `json:"recv_time_us"`
	SeqNum     uint16 `json:"seq_num"` // 该帧最后一个 RTP 包的序列号
}

// LostFrameInfo 描述一次丢帧事件
type LostFrameInfo struct {
	// 估算的丢帧发生时间（相对测试开始，ms）
	TimeOffsetMs float64 `json:"time_offset_ms"`
	// 连续丢失帧数估计
	Count int `json:"count"`
}

type TestResult struct {
	TestID         string    `json:"test_id"`
	Timestamp      time.Time `json:"timestamp"`
	DurationSec    int       `json:"duration_sec"`
	TotalBytesTx   int64     `json:"total_bytes_tx"`
	TotalBytesRx   int64     `json:"total_bytes_rx"`
	ThroughputMbps float64   `json:"throughput_mbps"`
	PacketsTx      int       `json:"packets_tx"`
	PacketsRx      int       `json:"packets_rx"`

	// Latency (per-frame, based on RTP Marker bit)
	AvgLatencyMs   float64   `json:"avg_latency_ms,omitempty"`
	P50LatencyMs   float64   `json:"p50_latency_ms,omitempty"`
	P95LatencyMs   float64   `json:"p95_latency_ms,omitempty"`
	P99LatencyMs   float64   `json:"p99_latency_ms,omitempty"`
	FrameCount     int       `json:"frame_count,omitempty"`
	FrameLatencies []float64 `json:"frame_latencies_ms,omitempty"`

	// 丢帧统计
	FramesSent      int             `json:"frames_sent"`
	FramesReceived  int             `json:"frames_received"`
	FramesLost      int             `json:"frames_lost"`
	FrameLossRate   float64         `json:"frame_loss_rate"` // 0.0~1.0
	LostFrameEvents []LostFrameInfo `json:"lost_frame_events,omitempty"`
}

var (
	resultMu      sync.Mutex
	bytesTx       int64
	bytesRx       int64
	packetsTx     int
	packetsRx     int
	testStartTime time.Time

	// 服务端：记录每帧（Marker=1 的 RTP 包）的到达时间和序列号
	srvFrameMu      sync.Mutex
	srvFrameRecords []FrameRecord
)

func getVideoFile() string {
	if v := os.Getenv("VIDEO_FILE"); v != "" {
		return v
	}
	return defaultVideoFile
}

func getVideoFPS() int {
	if v := os.Getenv("VIDEO_FPS"); v != "" {
		if fps, err := strconv.Atoi(v); err == nil && fps > 0 {
			return fps
		}
	}
	return defaultVideoFPS
}

func getTestDurationSec() int {
	if v := os.Getenv("TEST_DURATION"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return testDurationSec
}

// ---- server ------------------------------------------------------------------

func runServer() {
	listenAddr := "0.0.0.0:" + pionServerPort

	api, err := newServerAPI(listenAddr, pionServerIP)
	if err != nil {
		panic(err)
	}

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		panic(err)
	}

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		fmt.Println("[server] ICE state:", state)
	})

	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		fmt.Println("[server] Track received:",
			"kind=", track.Kind().String(),
			"codec=", track.Codec().MimeType)

		for {
			pkt, _, err := track.ReadRTP()
			tRecv := time.Now().UnixMicro()
			if err != nil {
				fmt.Println("[server] track read ended:", err)
				return
			}

			resultMu.Lock()
			bytesRx += int64(len(pkt.Payload))
			packetsRx++
			resultMu.Unlock()

			// Marker=1 表示一帧的最后一个 RTP 包到达，即一帧完整收到
			if pkt.Marker {
				srvFrameMu.Lock()
				srvFrameRecords = append(srvFrameRecords, FrameRecord{
					RecvTimeUs: tRecv,
					SeqNum:     pkt.SequenceNumber,
				})
				srvFrameMu.Unlock()
			}
		}
	})

	http.HandleFunc("/offer", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()

		var offer webrtc.SessionDescription
		if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
			http.Error(w, "decode offer failed: "+err.Error(), http.StatusBadRequest)
			return
		}

		if err := pc.SetRemoteDescription(offer); err != nil {
			http.Error(w, "set remote description failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		answer, err := pc.CreateAnswer(nil)
		if err != nil {
			http.Error(w, "create answer failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		gatherDone := webrtc.GatheringCompletePromise(pc)
		if err := pc.SetLocalDescription(answer); err != nil {
			http.Error(w, "set local description failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		<-gatherDone

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(pc.LocalDescription()); err != nil {
			http.Error(w, "encode answer failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	})

	// /frame_stats 现在返回完整的 FrameRecord 列表（含序列号）
	http.HandleFunc("/frame_stats", func(w http.ResponseWriter, r *http.Request) {
		srvFrameMu.Lock()
		out := make([]FrameRecord, len(srvFrameRecords))
		copy(out, srvFrameRecords)
		srvFrameMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(out); err != nil {
			http.Error(w, "encode frame_stats failed: "+err.Error(), http.StatusInternalServerError)
		}
	})

	fmt.Printf("[server] signaling HTTP on %s\n", signalingAddr)
	fmt.Printf("[server] pion TCP on %s:%s\n", pionServerIP, pionServerPort)

	if err := http.ListenAndServe(signalingAddr, nil); err != nil {
		panic(err)
	}
}

// ---- client ------------------------------------------------------------------

func runClient() {
	videoFile := getVideoFile()
	videoFPS := getVideoFPS()
	durationSec := getTestDurationSec()

	api, err := newClientAPI()
	if err != nil {
		panic(err)
	}

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		panic(err)
	}

	connectedChan := make(chan struct{})
	var connectedOnce sync.Once

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		fmt.Println("[client] ICE state:", state)

		if state == webrtc.ICEConnectionStateConnected {
			connectedOnce.Do(func() { close(connectedChan) })
		}

		if state == webrtc.ICEConnectionStateFailed {
			fmt.Println("[client] ICE failed, exiting")
			os.Exit(1)
		}
	})

	videoTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeH264,
			ClockRate: 90000,
		},
		"video",
		"pion",
	)
	if err != nil {
		panic(err)
	}

	rtpSender, err := pc.AddTrack(videoTrack)
	if err != nil {
		panic(err)
	}

	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, err := rtpSender.Read(rtcpBuf); err != nil {
				return
			}
		}
	}()

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		panic(err)
	}

	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		panic(err)
	}
	<-gatherDone

	offerJSON, err := json.Marshal(pc.LocalDescription())
	if err != nil {
		panic(err)
	}

	resp, err := http.Post("http://"+signalingAddr+"/offer", "application/json", bytes.NewReader(offerJSON))
	if err != nil {
		panic(fmt.Errorf("POST /offer failed: %w", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		panic(fmt.Errorf("POST /offer returned status %d: %s", resp.StatusCode, string(body)))
	}

	var answer webrtc.SessionDescription
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		panic(fmt.Errorf("decode answer failed: %w", err))
	}
	if err := pc.SetRemoteDescription(answer); err != nil {
		panic(err)
	}

	fmt.Println("[client] waiting for connection...")
	fmt.Printf("[client] test duration: %ds\n", durationSec)

	var sendTimes []int64

	select {
	case <-connectedChan:
		fmt.Println("[client] connected, start streaming video")
		fmt.Println("[client] video file:", videoFile)
		fmt.Println("[client] video fps:", videoFPS)

		testStartTime = time.Now()

		var streamErr error
		sendTimes, streamErr = streamH264(videoTrack, videoFile, videoFPS, durationSec)
		if streamErr != nil {
			fmt.Println("[client] stream error:", streamErr)
			os.Exit(1)
		}

		fmt.Println("[client] video streaming completed")

	case <-time.After(10 * time.Second):
		fmt.Println("[client] connection timeout")
		os.Exit(1)
	}

	time.Sleep(500 * time.Millisecond)

	lossResult := fetchFrameStats(sendTimes, videoFPS)

	resultMu.Lock()
	result := calculateResult(lossResult, durationSec)
	resultMu.Unlock()

	saveResult(result)

	fmt.Println("\n==================================================")
	fmt.Printf("✓ Video test completed successfully\n")
	fmt.Printf("  Throughput: %.2f Mbps\n", result.ThroughputMbps)
	fmt.Printf("  Packets: %d tx / %d rx\n", result.PacketsTx, result.PacketsRx)
	fmt.Printf("  Bytes: %d tx / %d rx\n", result.TotalBytesTx, result.TotalBytesRx)
	fmt.Printf("  Frames: %d sent / %d received / %d lost (%.1f%%)\n",
		result.FramesSent, result.FramesReceived, result.FramesLost,
		result.FrameLossRate*100)
	if result.FrameCount > 0 {
		fmt.Printf("  Latency (frames=%d): avg=%.1fms p50=%.1fms p95=%.1fms p99=%.1fms\n",
			result.FrameCount,
			result.AvgLatencyMs,
			result.P50LatencyMs,
			result.P95LatencyMs,
			result.P99LatencyMs,
		)
	}
	if len(result.LostFrameEvents) > 0 {
		fmt.Printf("  Loss events: %d burst(s)\n", len(result.LostFrameEvents))
		for i, e := range result.LostFrameEvents {
			fmt.Printf("    [%d] t=%.0fms, ~%d frame(s) lost\n", i+1, e.TimeOffsetMs, e.Count)
		}
	}
	fmt.Printf("  Results saved to: %s\n", os.Getenv("LOG_DIR"))

	_ = pc.Close()
	os.Exit(0)
}

// ---- streaming ---------------------------------------------------------------

func streamH264(track *webrtc.TrackLocalStaticSample, path string, fps int, durationSec int) ([]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open h264 file: %w", err)
	}
	defer f.Close()

	reader, err := h264reader.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("create h264 reader: %w", err)
	}

	frameDuration := time.Second / time.Duration(fps)
	ticker := time.NewTicker(frameDuration)
	defer ticker.Stop()

	endTime := time.Now().Add(time.Duration(durationSec) * time.Second)

	var sendTimes []int64

	for time.Now().Before(endTime) {
		nal, err := reader.NextNAL()
		if err == io.EOF {
			if _, err := f.Seek(0, 0); err != nil {
				return sendTimes, fmt.Errorf("seek h264 file: %w", err)
			}
			reader, err = h264reader.NewReader(f)
			if err != nil {
				return sendTimes, fmt.Errorf("recreate h264 reader: %w", err)
			}
			continue
		}
		if err != nil {
			return sendTimes, fmt.Errorf("read nal failed: %w", err)
		}

		<-ticker.C

		tSend := time.Now().UnixMicro()
		if err := track.WriteSample(media.Sample{
			Data:     nal.Data,
			Duration: frameDuration,
		}); err != nil {
			return sendTimes, fmt.Errorf("write sample failed: %w", err)
		}
		sendTimes = append(sendTimes, tSend)

		resultMu.Lock()
		bytesTx += int64(len(nal.Data))
		packetsTx++
		resultMu.Unlock()
	}

	return sendTimes, nil
}

// ---- 丢帧分析 ---------------------------------------------------------------

// frameLossResult 汇总丢帧分析结果
type frameLossResult struct {
	FrameLatMs      []float64
	FramesSent      int
	FramesReceived  int
	FramesLost      int
	FrameLossRate   float64
	LostFrameEvents []LostFrameInfo
}

// fetchFrameStats 从服务端拉取帧记录，分析丢帧情况
func fetchFrameStats(sendUs []int64, fps int) frameLossResult {
	resp, err := http.Get("http://" + signalingAddr + "/frame_stats")
	if err != nil {
		fmt.Println("[client] failed to fetch server frame stats:", err)
		return frameLossResult{FramesSent: len(sendUs)}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Printf("[client] GET /frame_stats returned status %d: %s\n", resp.StatusCode, string(body))
		return frameLossResult{FramesSent: len(sendUs)}
	}

	var records []FrameRecord
	if err := json.NewDecoder(resp.Body).Decode(&records); err != nil {
		fmt.Println("[client] failed to decode frame stats:", err)
		return frameLossResult{FramesSent: len(sendUs)}
	}

	sent := len(sendUs)
	recv := len(records)
	lost := sent - recv
	if lost < 0 {
		lost = 0
	}
	lossRate := 0.0
	if sent > 0 {
		lossRate = float64(lost) / float64(sent)
	}

	fmt.Printf("[client] frames: %d sent, %d received, %d lost (%.1f%%)\n",
		sent, recv, lost, lossRate*100)

	// ---- 计算帧延迟（按位置配对，丢帧时按 testStartTime 偏移对齐） ----
	// 用到达时间间隙来推断哪些位置发生了丢帧
	lostEvents := detectLossEvents(sendUs, records, fps)

	// 延迟配对：用实际到达的 recv 数量做上限
	n := recv
	if n > sent {
		n = sent
	}
	lats := make([]float64, 0, n)
	recvIdx := 0
	for sendIdx := 0; sendIdx < sent && recvIdx < recv; sendIdx++ {
		// 简单顺序配对（丢帧导致少量配对偏差，后续可用序列号改进）
		lats = append(lats, float64(records[recvIdx].RecvTimeUs-sendUs[sendIdx])/1000.0)
		recvIdx++
	}

	return frameLossResult{
		FrameLatMs:      lats,
		FramesSent:      sent,
		FramesReceived:  recv,
		FramesLost:      lost,
		FrameLossRate:   lossRate,
		LostFrameEvents: lostEvents,
	}
}

// detectLossEvents 通过分析服务端帧到达时间间隙，推断丢帧事件的时间位置和数量
//
// 原理：连续帧到达间隔期望值 = 1/fps 秒。
// 若间隔 > 1.5 * framePeriod，则认为中间有帧丢失，
// 丢失帧数 ≈ round(gap/framePeriod) - 1
func detectLossEvents(sendUs []int64, records []FrameRecord, fps int) []LostFrameInfo {
	if len(records) < 2 || fps <= 0 {
		return nil
	}

	framePeriodUs := int64(1_000_000 / fps) // 帧周期，微秒
	threshold := framePeriodUs * 3 / 2      // 超过 1.5 倍帧周期视为有丢帧

	testStartUs := int64(0)
	if len(sendUs) > 0 {
		testStartUs = sendUs[0]
	}

	var events []LostFrameInfo
	for i := 1; i < len(records); i++ {
		gap := records[i].RecvTimeUs - records[i-1].RecvTimeUs
		if gap > threshold {
			// 估算丢失帧数（gap 里有多少个"帧周期"，减去正常的那 1 帧）
			lostCount := int(gap/framePeriodUs) - 1
			if lostCount < 1 {
				lostCount = 1
			}
			// 丢帧事件发生时间：取上一帧到达后的时间点（相对测试开始，ms）
			offsetMs := float64(records[i-1].RecvTimeUs-testStartUs) / 1000.0
			events = append(events, LostFrameInfo{
				TimeOffsetMs: offsetMs,
				Count:        lostCount,
			})
		}
	}
	return events
}

// ---- percentile / result / save--------------------------

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := p / 100.0 * float64(len(sorted)-1)
	lo := int(idx)
	hi := lo + 1
	if hi >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	frac := idx - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

func calculateResult(lr frameLossResult, durationSec int) TestResult {
	duration := time.Since(testStartTime).Seconds()
	if duration <= 0 {
		duration = float64(durationSec)
	}

	totalBytes := bytesTx + bytesRx
	throughput := float64(totalBytes) * 8 / duration / 1e6

	r := TestResult{
		TestID:          fmt.Sprintf("webrtc-video-%d", time.Now().Unix()),
		Timestamp:       time.Now(),
		DurationSec:     durationSec,
		TotalBytesTx:    bytesTx,
		TotalBytesRx:    bytesRx,
		ThroughputMbps:  throughput,
		PacketsTx:       packetsTx,
		PacketsRx:       packetsRx,
		FramesSent:      lr.FramesSent,
		FramesReceived:  lr.FramesReceived,
		FramesLost:      lr.FramesLost,
		FrameLossRate:   lr.FrameLossRate,
		LostFrameEvents: lr.LostFrameEvents,
	}

	if len(lr.FrameLatMs) > 0 {
		sorted := make([]float64, len(lr.FrameLatMs))
		copy(sorted, lr.FrameLatMs)
		sort.Float64s(sorted)

		var sum float64
		for _, v := range sorted {
			sum += v
		}

		r.FrameCount     = len(lr.FrameLatMs)
		r.AvgLatencyMs   = sum / float64(len(sorted))
		r.P50LatencyMs   = percentile(sorted, 50)
		r.P95LatencyMs   = percentile(sorted, 95)
		r.P99LatencyMs   = percentile(sorted, 99)
		r.FrameLatencies = lr.FrameLatMs
	}

	return r
}

func saveResult(result TestResult) {
	logDir := os.Getenv("LOG_DIR")
	if logDir == "" {
		cwd, _ := os.Getwd()
		if filepath.Base(cwd) == "pion" {
			logDir = filepath.Join(filepath.Dir(cwd), "logs", "webrtc")
		} else {
			logDir = filepath.Join(cwd, "logs", "webrtc")
		}
	}

	if err := os.MkdirAll(logDir, 0755); err != nil {
		panic(err)
	}

	jsonFile, err := os.Create(filepath.Join(logDir, "results.json"))
	if err != nil {
		panic(err)
	}
	defer jsonFile.Close()

	encoder := json.NewEncoder(jsonFile)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		panic(err)
	}

	summaryFile, err := os.Create(filepath.Join(logDir, "summary.txt"))
	if err != nil {
		panic(err)
	}
	defer summaryFile.Close()

	fmt.Fprintf(summaryFile, "Test ID: %s\n", result.TestID)
	fmt.Fprintf(summaryFile, "Timestamp: %s\n", result.Timestamp.Format(time.RFC3339))
	fmt.Fprintf(summaryFile, "Duration: %d seconds\n", result.DurationSec)
	fmt.Fprintf(summaryFile, "Throughput: %.2f Mbps\n", result.ThroughputMbps)
	fmt.Fprintf(summaryFile, "Packets: %d tx / %d rx\n", result.PacketsTx, result.PacketsRx)
	fmt.Fprintf(summaryFile, "Bytes: %d tx / %d rx\n", result.TotalBytesTx, result.TotalBytesRx)
	fmt.Fprintf(summaryFile, "Frames: %d sent / %d received / %d lost (%.1f%%)\n",
		result.FramesSent, result.FramesReceived, result.FramesLost,
		result.FrameLossRate*100)
	if len(result.LostFrameEvents) > 0 {
		fmt.Fprintf(summaryFile, "Loss events:\n")
		for i, e := range result.LostFrameEvents {
			fmt.Fprintf(summaryFile, "  [%d] t=%.0fms, ~%d frame(s)\n", i+1, e.TimeOffsetMs, e.Count)
		}
	}
	if result.FrameCount > 0 {
		fmt.Fprintf(summaryFile, "Frames measured: %d\n", result.FrameCount)
		fmt.Fprintf(summaryFile, "Latency avg: %.2f ms\n", result.AvgLatencyMs)
		fmt.Fprintf(summaryFile, "Latency p50: %.2f ms\n", result.P50LatencyMs)
		fmt.Fprintf(summaryFile, "Latency p95: %.2f ms\n", result.P95LatencyMs)
		fmt.Fprintf(summaryFile, "Latency p99: %.2f ms\n", result.P99LatencyMs)
	}
}

// ---- main --------------------------------------------------------------------

func main() {
	role := flag.String("role", "", "server or client")
	flag.Parse()

	switch *role {
	case "server":
		runServer()
	case "client":
		runClient()
	default:
		fmt.Fprintln(os.Stderr, "Usage: -role server or -role client")
		os.Exit(1)
	}
}
