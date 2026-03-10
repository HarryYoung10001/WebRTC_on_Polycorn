package main

import (
	"fmt"
	"net"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v3"
)

func buildAPI(s webrtc.SettingEngine) (*webrtc.API, error) {
	var m webrtc.MediaEngine
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("register default codecs: %w", err)
	}

	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(&m, ir); err != nil {
		return nil, fmt.Errorf("register default interceptors: %w", err)
	}

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(&m),
		webrtc.WithInterceptorRegistry(ir),
		webrtc.WithSettingEngine(s),
	)
	return api, nil
}

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
	s.SetNAT1To1IPs([]string{announceIP}, webrtc.ICECandidateTypeHost)

	return buildAPI(s)
}

// newClientAPI 给 client 用：不监听固定端口，只负责配置 TCP 网络类型
func newClientAPI() (*webrtc.API, error) {
	tcpListener, err := net.ListenTCP("tcp4", &net.TCPAddr{Port: 0})
	if err != nil {
		return nil, fmt.Errorf("listen TCP: %w", err)
	}

	tcpMux := webrtc.NewICETCPMux(nil, tcpListener, 8)

	s := webrtc.SettingEngine{}
	s.SetICETCPMux(tcpMux)
	s.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeTCP4})

	return buildAPI(s)
}
