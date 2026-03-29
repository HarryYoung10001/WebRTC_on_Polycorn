// demo/main.go — Polycorn++ Demo Server
//
// 启动：
//   cd demo/
//   go build -o demo_server .
//   DEMO_LOGS=../logs ./demo_server
//
// 环境变量：
//   DEMO_ADDR    监听地址，默认 :8888
//   DEMO_LOGS    实验日志根目录，默认 ../logs
//   DEMO_MEDIA   视频文件目录（.mp4/.webm），默认 ../pion/media

package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ── results.json 结构（与 pion/main.go 中 TestResult 对齐）────────────────────

type lostFrameEvent struct {
	TimeOffsetMs float64 `json:"time_offset_ms"`
	Count        int     `json:"count"`
}

type resultJSON struct {
	ThroughputMbps float64          `json:"throughput_mbps"`
	AvgLatencyMs   float64          `json:"avg_latency_ms"`
	P99LatencyMs   float64          `json:"p99_latency_ms"`
	FrameCount     int              `json:"frame_count"`
	FrameLatencies []float64        `json:"frame_latencies_ms"`
	// 丢帧字段（新版 pion/main.go 写入；旧格式缺失时按无丢帧处理）
	FramesSent      int              `json:"frames_sent"`
	FramesReceived  int              `json:"frames_received"`
	FramesLost      int              `json:"frames_lost"`
	FrameLossRate   float64          `json:"frame_loss_rate"`
	LostFrameEvents []lostFrameEvent `json:"lost_frame_events"`
}

// ── API 响应结构 ──────────────────────────────────────────────────────────────

type traceEntry struct {
	Trace          string  `json:"trace"`
	AvgLatencyMs   float64 `json:"avg_latency_ms"`
	ThroughputMbps float64 `json:"throughput_mbps"`
	P99Ms          float64 `json:"p99_ms"`
	FrameLossRate  float64 `json:"frame_loss_rate"`
	FramesLost     int     `json:"frames_lost"`
}

// wsFrame 是每帧推送给前端的 WebSocket 消息。
//
//   正常帧：Freeze=false，携带 LatencyMs/AvgMs。
//   冻帧事件：Freeze=true，携带 FreezeFrames（burst 中连续丢帧总数）。
//     服务端仍以 30fps 消耗 ticker（保证回放节奏），但一个 burst 只发一条消息；
//     前端据此冻结右视频 FreezeFrames/fps 秒。
type wsFrame struct {
	Frame        int     `json:"frame"`
	LatencyMs    float64 `json:"latency_ms,omitempty"`
	AvgMs        float64 `json:"avg_ms,omitempty"`
	Freeze       bool    `json:"freeze,omitempty"`
	FreezeFrames int     `json:"freeze_frames,omitempty"`
	LossRate     float64 `json:"loss_rate"` // 累计丢帧率，每帧更新
}

// ── main ──────────────────────────────────────────────────────────────────────

func main() {
	addr     := envOr("DEMO_ADDR",  ":8888")
	logsDir  := envOr("DEMO_LOGS",  "../logs")
	mediaDir := envOr("DEMO_MEDIA", "../pion/media")

	mux := http.NewServeMux()

	mux.Handle("/", http.FileServer(http.Dir(".")))

	mux.HandleFunc("/video", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, "missing ?name=", http.StatusBadRequest)
			return
		}
		for _, ext := range []string{".mp4", ".webm"} {
			p := filepath.Join(mediaDir, name+ext)
			if _, err := os.Stat(p); err == nil {
				w.Header().Set("Cache-Control", "public, max-age=3600")
				http.ServeFile(w, r, p)
				return
			}
		}
		http.Error(w, "video not found: "+name, http.StatusNotFound)
	})

	mux.HandleFunc("/api/media", func(w http.ResponseWriter, r *http.Request) {
		entries, _ := os.ReadDir(mediaDir)
		seen := map[string]bool{}
		var names []string
		for _, e := range entries {
			for _, ext := range []string{".mp4", ".webm"} {
				if strings.HasSuffix(e.Name(), ext) {
					base := strings.TrimSuffix(e.Name(), ext)
					if !seen[base] {
						seen[base] = true
						names = append(names, base)
					}
				}
			}
		}
		sort.Strings(names)
		jsonResp(w, names)
	})

	// /api/traces — 含丢帧率信息
	mux.HandleFunc("/api/traces", func(w http.ResponseWriter, r *http.Request) {
		matches, _ := filepath.Glob(
			filepath.Join(logsDir, "webrtc-trace", "*", "results.json"),
		)
		var list []traceEntry
		for _, m := range matches {
			data, err := os.ReadFile(m)
			if err != nil {
				continue
			}
			var res resultJSON
			if err := json.Unmarshal(data, &res); err != nil {
				continue
			}
			if (res.FrameCount == 0 && len(res.FrameLatencies) == 0) || res.AvgLatencyMs <= 0 {
				continue
			}
			list = append(list, traceEntry{
				Trace:          filepath.Base(filepath.Dir(m)),
				AvgLatencyMs:   res.AvgLatencyMs,
				ThroughputMbps: res.ThroughputMbps,
				P99Ms:          res.P99LatencyMs,
				FrameLossRate:  res.FrameLossRate,
				FramesLost:     res.FramesLost,
			})
		}
		sort.Slice(list, func(i, j int) bool {
			return list[i].AvgLatencyMs < list[j].AvgLatencyMs
		})
		jsonResp(w, list)
	})

	// /ws?trace=... — WebSocket 回放，内嵌冻帧事件
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		traceName := r.URL.Query().Get("trace")
		if traceName == "" {
			http.Error(w, "missing ?trace=", http.StatusBadRequest)
			return
		}

		resultPath := filepath.Join(logsDir, "webrtc-trace", traceName, "results.json")
		data, err := os.ReadFile(resultPath)
		if err != nil {
			http.Error(w, "results not found: "+traceName, http.StatusNotFound)
			return
		}
		var res resultJSON
		if err := json.Unmarshal(data, &res); err != nil || len(res.FrameLatencies) == 0 {
			http.Error(w, "no frame latency data", http.StatusNotFound)
			return
		}

		conn, err := wsHandshake(w, r)
		if err != nil {
			log.Println("[demo] ws handshake:", err)
			return
		}
		defer conn.Close()

		// ── 重建含冻帧事件的完整帧时间线 ──────────────────────────────
		const fps         = 30
		const windowSize  = 60
		const framePeriod = 1000.0 / float64(fps) // 每帧毫秒数

		// 总发送帧数：优先 FramesSent，兼容旧格式
		totalSent := res.FramesSent
		if totalSent == 0 {
			totalSent = len(res.FrameLatencies)
		}

		// 将 LostFrameEvents（时间偏移 ms）转换为帧序号区间
		// time_offset_ms 是最后一个正常帧的接收时刻偏移，近似等于帧序号位置
		type lossRange struct{ start, end, count int }
		var lossRanges []lossRange
		for _, e := range res.LostFrameEvents {
			start := int(e.TimeOffsetMs / framePeriod)
			if start < 0 {
				start = 0
			}
			end := start + e.Count - 1
			if end >= totalSent {
				end = totalSent - 1
			}
			lossRanges = append(lossRanges, lossRange{start, end, e.Count})
		}
		sort.Slice(lossRanges, func(i, j int) bool {
			return lossRanges[i].start < lossRanges[j].start
		})

		// 构建快查表：O(1) 判断某帧是否为冻帧、是否为 burst 首帧
		inFreeze    := make(map[int]bool, res.FramesLost)
		freezeStart := make(map[int]int, len(lossRanges)) // frameIdx -> burstCount
		for _, lr := range lossRanges {
			freezeStart[lr.start] = lr.count
			for fi := lr.start; fi <= lr.end; fi++ {
				inFreeze[fi] = true
			}
		}

		log.Printf("[demo] replay: trace=%s total=%d lost=%d bursts=%d",
			traceName, totalSent, res.FramesLost, len(lossRanges))

		// ── 回放循环 ──────────────────────────────────────────────────
		ticker := time.NewTicker(time.Second / fps)
		defer ticker.Stop()

		var window  []float64
		recvIdx    := 0
		lostSoFar  := 0

		for i := 0; i < totalSent; i++ {
			<-ticker.C

			if inFreeze[i] {
				lostSoFar++
				// 仅 burst 首帧发送冻帧消息，其余静默消耗 ticker 保持节奏
				if count, isStart := freezeStart[i]; isStart {
					lossRate := math.Round(float64(lostSoFar)/float64(i+1)*1000) / 1000
					msg, _ := json.Marshal(wsFrame{
						Frame:        i + 1,
						Freeze:       true,
						FreezeFrames: count,
						LossRate:     lossRate,
					})
					if err := wsSendText(conn, msg); err != nil {
						log.Println("[demo] ws send:", err)
						return
					}
				}
				continue
			}

			// 正常接收帧
			lat := 0.0
			if recvIdx < len(res.FrameLatencies) {
				lat = res.FrameLatencies[recvIdx]
				recvIdx++
			}
			window = append(window, lat)
			if len(window) > windowSize {
				window = window[1:]
			}
			lossRate := 0.0
			if i > 0 {
				lossRate = math.Round(float64(lostSoFar)/float64(i+1)*1000) / 1000
			}

			msg, _ := json.Marshal(wsFrame{
				Frame:     i + 1,
				LatencyMs: lat,
				AvgMs:     rollingMean(window),
				LossRate:  lossRate,
			})
			if err := wsSendText(conn, msg); err != nil {
				log.Println("[demo] ws send:", err)
				return
			}
		}

		// 结束哨兵
		end, _ := json.Marshal(wsFrame{Frame: 0})
		_ = wsSendText(conn, end)
		log.Printf("[demo] replay done: trace=%s sent=%d lost=%d", traceName, recvIdx, lostSoFar)
	})

	log.Printf("[demo] listening on http://localhost%s", addr)
	log.Printf("[demo] logs  : %s", logsDir)
	log.Printf("[demo] media : %s", mediaDir)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

// ── WebSocket 实现（RFC 6455），零外部依赖 ───────────────────────────────

func wsHandshake(w http.ResponseWriter, r *http.Request) (net.Conn, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	const magic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	h := sha1.Sum([]byte(key + magic))
	accept := base64.StdEncoding.EncodeToString(h[:])

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return nil, nil
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return nil, err
	}

	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := buf.WriteString(resp); err != nil {
		conn.Close()
		return nil, err
	}
	if err := buf.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func wsSendText(conn net.Conn, payload []byte) error {
	n := len(payload)
	var header []byte
	switch {
	case n <= 125:
		header = []byte{0x81, byte(n)}
	case n <= 65535:
		header = make([]byte, 4)
		header[0] = 0x81
		header[1] = 126
		binary.BigEndian.PutUint16(header[2:], uint16(n))
	default:
		header = make([]byte, 10)
		header[0] = 0x81
		header[1] = 127
		binary.BigEndian.PutUint64(header[2:], uint64(n))
	}
	if _, err := conn.Write(header); err != nil {
		return err
	}
	_, err := conn.Write(payload)
	return err
}

// ── utils ──────────────────────────────────────────────────────────────────

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func jsonResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(v)
}

func rollingMean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return math.Round(s/float64(len(xs))*10) / 10
}
