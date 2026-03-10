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
	testDurationSec = 45

	defaultVideoFile = "media/bbb_45s.h264"
	defaultVideoFPS  = 30
)

var signalingAddr = func() string {
	if v := os.Getenv("SIGNAL_ADDR"); v != "" {
		return v
	}
	return "127.0.0.1:8080"
}()

type TestResult struct {
	TestID         string    `json:"test_id"`
	Timestamp      time.Time `json:"timestamp"`
	DurationSec    int       `json:"duration_sec"`
	TotalBytesTx   int64     `json:"total_bytes_tx"`
	TotalBytesRx   int64     `json:"total_bytes_rx"`
	ThroughputMbps float64   `json:"throughput_mbps"`
	PacketsTx      int       `json:"packets_tx"`
	PacketsRx      int       `json:"packets_rx"`
}

var (
	resultMu      sync.Mutex
	bytesTx       int64
	bytesRx       int64
	packetsTx     int
	packetsRx     int
	testStartTime time.Time
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
			if err != nil {
				fmt.Println("[server] track read ended:", err)
				return
			}

			resultMu.Lock()
			bytesRx += int64(len(pkt.Payload))
			packetsRx++
			resultMu.Unlock()
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

	fmt.Printf("[server] signaling HTTP on %s\n", signalingAddr)
	fmt.Printf("[server] pion TCP on %s:%s\n", pionServerIP, pionServerPort)

	if err := http.ListenAndServe(signalingAddr, nil); err != nil {
		panic(err)
	}
}

func runClient() {
	videoFile := getVideoFile()
	videoFPS := getVideoFPS()

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
		panic(err)
	}
	defer resp.Body.Close()

	var answer webrtc.SessionDescription
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		panic(err)
	}
	if err := pc.SetRemoteDescription(answer); err != nil {
		panic(err)
	}

	fmt.Println("[client] waiting for connection...")

	select {
	case <-connectedChan:
		fmt.Println("[client] connected, start streaming video")
		fmt.Println("[client] video file:", videoFile)
		fmt.Println("[client] video fps:", videoFPS)

		testStartTime = time.Now()

		if err := streamH264(videoTrack, videoFile, videoFPS, testDurationSec); err != nil {
			fmt.Println("[client] stream error:", err)
			os.Exit(1)
		}

		fmt.Println("[client] video streaming completed")

	case <-time.After(10 * time.Second):
		fmt.Println("[client] connection timeout")
		os.Exit(1)
	}

	resultMu.Lock()
	result := calculateResult()
	resultMu.Unlock()

	saveResult(result)

	fmt.Println("\n==================================================")
	fmt.Printf("✓ Video test completed successfully\n")
	fmt.Printf("  Throughput: %.2f Mbps\n", result.ThroughputMbps)
	fmt.Printf("  Packets: %d tx / %d rx\n", result.PacketsTx, result.PacketsRx)
	fmt.Printf("  Bytes: %d tx / %d rx\n", result.TotalBytesTx, result.TotalBytesRx)
	fmt.Printf("  Results saved to: logs/webrtc/results.json\n")

	_ = pc.Close()
	os.Exit(0)
}

func streamH264(track *webrtc.TrackLocalStaticSample, path string, fps int, durationSec int) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open h264 file: %w", err)
	}
	defer f.Close()

	reader, err := h264reader.NewReader(f)
	if err != nil {
		return fmt.Errorf("create h264 reader: %w", err)
	}

	frameDuration := time.Second / time.Duration(fps)
	ticker := time.NewTicker(frameDuration)
	defer ticker.Stop()

	endTime := time.Now().Add(time.Duration(durationSec) * time.Second)

	for time.Now().Before(endTime) {
		nal, err := reader.NextNAL()
		if err == io.EOF {
			if _, err := f.Seek(0, 0); err != nil {
				return fmt.Errorf("seek h264 file: %w", err)
			}
			reader, err = h264reader.NewReader(f)
			if err != nil {
				return fmt.Errorf("recreate h264 reader: %w", err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("read nal failed: %w", err)
		}

		<-ticker.C

		if err := track.WriteSample(media.Sample{
			Data:     nal.Data,
			Duration: frameDuration,
		}); err != nil {
			return fmt.Errorf("write sample failed: %w", err)
		}

		resultMu.Lock()
		bytesTx += int64(len(nal.Data))
		packetsTx++
		resultMu.Unlock()
	}

	return nil
}

func calculateResult() TestResult {
	duration := time.Since(testStartTime).Seconds()
	if duration <= 0 {
		duration = float64(testDurationSec)
	}

	totalBytes := bytesTx + bytesRx
	throughput := float64(totalBytes) * 8 / duration / 1e6

	return TestResult{
		TestID:         fmt.Sprintf("webrtc-video-%d", time.Now().Unix()),
		Timestamp:      time.Now(),
		DurationSec:    testDurationSec,
		TotalBytesTx:   bytesTx,
		TotalBytesRx:   bytesRx,
		ThroughputMbps: throughput,
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
