package main

import (
	"fmt"
	"net"

	"github.com/pion/webrtc/v3"
)

// newServerAPI 给 server 用：监听在 listenAddr，ICE candidate 对外宣告 announceIP
func newServerAPI(listenAddr string, announceIP string) (*webrtc.API, error) {
	addr, err := net.ResolveTCPAddr("tcp4", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve listen addr: %w", err)
	}

	tcpListener, err := net.ListenTCP("tcp4", addr)
	if err != nil {
		return nil, fmt.Errorf("listen TCP: %w", err)
	}

	fmt.Printf("[transport] server TCP listening on %s\n", tcpListener.Addr())

	tcpMux := webrtc.NewICETCPMux(nil, tcpListener, 8)

	s := webrtc.SettingEngine{}
	s.SetICETCPMux(tcpMux)
	s.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeTCP4})
	// 告诉 pion 对外宣告的 IP 是 announceIP（127.0.0.2）
	s.SetNAT1To1IPs([]string{announceIP}, webrtc.ICECandidateTypeHost)

	return webrtc.NewAPI(webrtc.WithSettingEngine(s)), nil
}

// newClientAPI 给 client 用：不监听，只负责配置 TCP 网络类型
func newClientAPI() (*webrtc.API, error) {
	// client 侧让 pion 自己发起 TCP connect
	// 连接目标 127.0.0.2:5004 会被 iptables 重定向到 polycorn client
	tcpListener, err := net.ListenTCP("tcp4", &net.TCPAddr{Port: 0})
	if err != nil {
		return nil, fmt.Errorf("listen TCP: %w", err)
	}

	tcpMux := webrtc.NewICETCPMux(nil, tcpListener, 8)

	s := webrtc.SettingEngine{}
	s.SetICETCPMux(tcpMux)
	s.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeTCP4})

	return webrtc.NewAPI(webrtc.WithSettingEngine(s)), nil
}
