package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"
)

const (
	signalingAddr   = "127.0.0.1:8080"
	pionServerIP    = "127.0.0.2"
	pionServerPort  = "5004"
	testDurationSec = 30
	packetInterval  = 10 * time.Millisecond
	payloadSize     = 1400
)

type TestResult struct {
	TestID         string    `json:"test_id"`
	Timestamp      time.Time `json:"timestamp"`
	DurationSec    int       `json:"duration_sec"`
	TotalBytesTx   int64     `json:"total_bytes_tx"`
	TotalBytesRx   int64     `json:"total_bytes_rx"`
	ThroughputMbps float64   `json:"throughput_mbps"`
	LatencyMs      []float64 `json:"latency_ms"`
	AvgLatencyMs   float64   `json:"avg_latency_ms"`
	MinLatencyMs   float64   `json:"min_latency_ms"`
	MaxLatencyMs   float64   `json:"max_latency_ms"`
	PacketsTx      int       `json:"packets_tx"`
	PacketsRx      int       `json:"packets_rx"`
}

var (
	resultMu      sync.Mutex
	bytesTx       int64
	bytesRx       int64
	packetsTx     int
	packetsRx     int
	latencies     []float64
	testStartTime time.Time
)

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

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		fmt.Println("[server] DataChannel opened:", dc.Label())

		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if len(msg.Data) >= 8 {
				sentTime := time.Unix(0, int64(binary.BigEndian.Uint64(msg.Data[:8])))
				latency := time.Since(sentTime).Seconds() * 1000

				resultMu.Lock()
				latencies = append(latencies, latency)
				bytesRx += int64(len(msg.Data))
				packetsRx++
				resultMu.Unlock()

				response := make([]byte, 8+len(msg.Data[8:]))
				copy(response[:8], msg.Data[:8])
				copy(response[8:], []byte("pong"))
				dc.Send(response)
			}
		})
	})

	http.HandleFunc("/offer", func(w http.ResponseWriter, r *http.Request) {
		var offer webrtc.SessionDescription
		json.NewDecoder(r.Body).Decode(&offer)
		pc.SetRemoteDescription(offer)
		answer, _ := pc.CreateAnswer(nil)
		gatherDone := webrtc.GatheringCompletePromise(pc)
		pc.SetLocalDescription(answer)
		<-gatherDone
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pc.LocalDescription())
	})

	fmt.Printf("[server] signaling HTTP on %s\n", signalingAddr)
	fmt.Printf("[server] pion TCP on %s:%s\n", pionServerIP, pionServerPort)
	http.ListenAndServe(signalingAddr, nil)
}

func runClient() {
	api, err := newClientAPI()
	if err != nil {
		panic(err)
	}

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		panic(err)
	}

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		fmt.Println("[client] ICE state:", state)
		if state == webrtc.ICEConnectionStateFailed {
			fmt.Println("[client] ICE failed, exiting")
			os.Exit(1)
		}
	})

	dc, err := pc.CreateDataChannel("test", nil)
	if err != nil {
		panic(err)
	}

	// announce test completed by channel
	doneChan := make(chan struct{})
	var closeOnce sync.Once
	closeDone := func() { closeOnce.Do(func() { close(doneChan) }) }
	testFailed := false

	dc.OnOpen(func() {
		fmt.Println("[client] DataChannel open, starting measurement...")
		testStartTime = time.Now()

		go func() {
			ticker := time.NewTicker(packetInterval)
			defer ticker.Stop()

			payload := make([]byte, payloadSize)
			endTime := time.Now().Add(time.Duration(testDurationSec) * time.Second)

			for time.Now().Before(endTime) {
				<-ticker.C
				timestamp := make([]byte, 8)
				binary.BigEndian.PutUint64(timestamp, uint64(time.Now().UnixNano()))

				buf := append(timestamp, payload...)
				if err := dc.Send(buf); err != nil {
					fmt.Println("[client] send error:", err)
					return
				}
				resultMu.Lock()
				bytesTx += int64(len(buf))
				packetsTx++
				resultMu.Unlock()
			}
		}()

		// acknowledge after test
		go func() {
			time.Sleep(time.Duration(testDurationSec+2) * time.Second)
			closeDone()
		}()
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if len(msg.Data) >= 8 {
			sentTime := time.Unix(0, int64(binary.BigEndian.Uint64(msg.Data[:8])))
			latency := time.Since(sentTime).Seconds() * 1000

			resultMu.Lock()
			latencies = append(latencies, latency)
			bytesRx += int64(len(msg.Data))
			packetsRx++
			resultMu.Unlock()
		}
	})

	dc.OnClose(func() {
		fmt.Println("[client] DataChannel closed")
		if !testFailed {
			closeDone()
		}
	})

	// give offer
	offer, _ := pc.CreateOffer(nil)
	gatherDone := webrtc.GatheringCompletePromise(pc)
	pc.SetLocalDescription(offer)
	<-gatherDone

	offerJSON, _ := json.Marshal(pc.LocalDescription())
	resp, _ := http.Post("http://"+signalingAddr+"/offer", "application/json", bytes.NewReader(offerJSON))
	defer resp.Body.Close()

	var answer webrtc.SessionDescription
	json.NewDecoder(resp.Body).Decode(&answer)
	pc.SetRemoteDescription(answer)

	fmt.Println("[client] waiting for connection...")

	// waiting for test completed or timeout
	select {
	case <-doneChan:
		fmt.Println("[client] test completed")
	case <-time.After(time.Duration(testDurationSec+10) * time.Second):
		fmt.Println("[client] test timeout")
		testFailed = true
	}

	// calculate and memory
	resultMu.Lock()
	result := calculateResult()
	resultMu.Unlock()

	saveResult(result)

	fmt.Println("\n==================================================")
	fmt.Printf("✓ Test completed successfully\n")
	fmt.Printf("  Throughput: %.2f Mbps\n", result.ThroughputMbps)
	fmt.Printf("  Avg Latency: %.2f ms\n", result.AvgLatencyMs)
	fmt.Printf("  Packets: %d tx / %d rx\n", result.PacketsTx, result.PacketsRx)
	fmt.Printf("  Results saved to: logs/webrtc/results.json\n")

	pc.Close()
	os.Exit(0)
}

func calculateResult() TestResult {
	duration := time.Since(testStartTime).Seconds()
	if duration <= 0 {
		duration = float64(testDurationSec)
	}
	totalBytes := bytesTx + bytesRx
	throughput := float64(totalBytes) * 8 / duration / 1e6

	avgLatency := 0.0
	minLatency := 999999.0
	maxLatency := 0.0
	for _, l := range latencies {
		avgLatency += l
		if l < minLatency {
			minLatency = l
		}
		if l > maxLatency {
			maxLatency = l
		}
	}
	if len(latencies) > 0 {
		avgLatency /= float64(len(latencies))
	} else {
		minLatency = 0
	}

	return TestResult{
		TestID:         fmt.Sprintf("webrtc-%d", time.Now().Unix()),
		Timestamp:      time.Now(),
		DurationSec:    testDurationSec,
		TotalBytesTx:   bytesTx,
		TotalBytesRx:   bytesRx,
		ThroughputMbps: throughput,
		LatencyMs:      latencies,
		AvgLatencyMs:   avgLatency,
		MinLatencyMs:   minLatency,
		MaxLatencyMs:   maxLatency,
		PacketsTx:      packetsTx,
		PacketsRx:      packetsRx,
	}
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

	os.MkdirAll(logDir, 0755)

	jsonFile, _ := os.Create(filepath.Join(logDir, "results.json"))
	defer jsonFile.Close()
	encoder := json.NewEncoder(jsonFile)
	encoder.SetIndent("", "  ")
	encoder.Encode(result)

	summaryFile, _ := os.Create(filepath.Join(logDir, "summary.txt"))
	defer summaryFile.Close()
	fmt.Fprintf(summaryFile, "Test ID: %s\n", result.TestID)
	fmt.Fprintf(summaryFile, "Timestamp: %s\n", result.Timestamp.Format(time.RFC3339))
	fmt.Fprintf(summaryFile, "Duration: %d seconds\n", result.DurationSec)
	fmt.Fprintf(summaryFile, "Throughput: %.2f Mbps\n", result.ThroughputMbps)
	fmt.Fprintf(summaryFile, "Avg Latency: %.2f ms\n", result.AvgLatencyMs)
	fmt.Fprintf(summaryFile, "Min/Max Latency: %.2f / %.2f ms\n", result.MinLatencyMs, result.MaxLatencyMs)
	fmt.Fprintf(summaryFile, "Packets: %d tx / %d rx\n", result.PacketsTx, result.PacketsRx)
	fmt.Fprintf(summaryFile, "Bytes: %d tx / %d rx\n", result.TotalBytesTx, result.TotalBytesRx)
}

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