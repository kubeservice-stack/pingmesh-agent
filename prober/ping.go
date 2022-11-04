// Copyright 2022 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prober

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/time/rate"

	"github.com/kubeservice-stack/pingmesh-agent/config"
)

var (
	pingDurationGaugeVec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pingmesh_duration_milliseconds",
			Help: "duration of ping rtt",
		},
		[]string{"target", "tor"},
	)

	pingFailGaugeVec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pingmesh_fail",
			Help: "ping fail",
		},
		[]string{"target", "tor"},
	)
)

func init() {
	prometheus.MustRegister(pingDurationGaugeVec)
	prometheus.MustRegister(pingFailGaugeVec)
}

type Service interface {
	Start()
	Stop()
}

type PingSchedule struct {
	Conf            *config.SafeConfig
	Logger          log.Logger
	Limit           int
	Interval        time.Duration
	MaxDeley        time.Duration
	Timeout         time.Duration
	IPProtocol      string
	SourceIPAddress string
	StopCh          chan bool
}

func NewPingSchedule(sc *config.SafeConfig, concurrentLimit uint,
	interval, maxDeley, timeout time.Duration, protocal string, ipaddr string, logger log.Logger) Service {
	return &PingSchedule{
		Conf:            sc,
		Logger:          logger,
		Limit:           int(concurrentLimit),
		Interval:        interval,
		MaxDeley:        maxDeley,
		Timeout:         timeout,
		IPProtocol:      protocal,
		SourceIPAddress: ipaddr,
		StopCh:          make(chan bool, 1),
	}
}

func (ps *PingSchedule) reset() {
	pingDurationGaugeVec.Reset()
	pingFailGaugeVec.Reset()
}

func (ps *PingSchedule) Stop() {
	ps.StopCh <- true
}

func (ps *PingSchedule) Start() {
	ticker := time.NewTicker(ps.Interval)
	defer ticker.Stop()

	logger := ps.Logger

	for {
		ps.Conf.RLock()
		pinglist := ps.Conf.P
		if pinglist.IsNew {
			ps.reset()
			ps.Conf.P.IsNew = false
		}
		ps.Conf.RUnlock()

		level.Info(logger).Log("pingAll", "start pingAll")
		startTime := time.Now().Unix()
		ps.pingAll(pinglist.Items)
		level.Info(logger).Log("pingAll", "end pingAll", "duration(second)", time.Now().Unix()-startTime)

		select {
		case <-ps.StopCh:
			return
		case <-ticker.C:
		}
	}
}

type IpTor struct {
	Ip   string //v4, v6, domain
	Name string
}

func (ps *PingSchedule) pingAll(items map[string]config.PingMeshItem) {
	var iplist []IpTor
	var ipsum int
	for _, item := range items {
		for _, v := range item.IPs {
			iplist = append(iplist, IpTor{Ip: v, Name: item.Name})
		}
		ipsum = ipsum + len(item.IPs)
	}
	limiter := pingRate(ipsum, ps.Interval, ps.Limit)
	concurrent := make(chan bool, int(ps.Limit))

	model := config.Module{
		Prober: "icmp",
		ICMP: config.ICMPProbe{
			IPProtocol:         ps.IPProtocol,
			IPProtocolFallback: true,
			SourceIPAddress:    ps.SourceIPAddress,
		},
	}

	var wg sync.WaitGroup

	for _, iptor := range iplist {
		concurrent <- true
		limiter.Wait(context.Background())
		wg.Add(1)

		go func(ipTor IpTor) {
			ctx, cancel := context.WithTimeout(context.Background(), ps.Timeout)
			defer cancel()
			defer wg.Done()

			success, rtt := ps.pingOne(ctx, ipTor.Ip, model)
			if !success {
				pingFailGaugeVec.WithLabelValues(ipTor.Ip, ipTor.Name).Set(1)
			} else {
				pingFailGaugeVec.DeleteLabelValues(ipTor.Ip, ipTor.Name)

				if rtt > 1000.0*ps.MaxDeley.Seconds() {
					pingDurationGaugeVec.WithLabelValues(ipTor.Ip, ipTor.Name).Set(rtt)
				} else {
					pingDurationGaugeVec.DeleteLabelValues(ipTor.Ip, ipTor.Name)
				}
			}

			<-concurrent

		}(iptor)
	}

	wg.Wait()
}

func (ps *PingSchedule) pingOne(ctx context.Context, addr string, module config.Module) (success bool, rtt float64) {
	var (
		socket      net.PacketConn
		requestType icmp.Type
		replyType   icmp.Type
	)
	logger := ps.Logger

	level.Info(logger).Log("msg", "start ping a addr", "addr", addr)
	ip, _, err := pingChooseProtocol(ctx, module.ICMP.IPProtocol, module.ICMP.IPProtocolFallback, addr, logger)
	if err != nil {
		level.Warn(logger).Log("msg", "Error resolving address", "err", err)
		return
	}

	var srcIP net.IP
	if len(module.ICMP.SourceIPAddress) > 0 {
		if srcIP = net.ParseIP(module.ICMP.SourceIPAddress); srcIP == nil {
			level.Error(logger).Log("msg", "Error parsing source ip address", "srcIP", module.ICMP.SourceIPAddress)
			return
		}
		level.Info(logger).Log("msg", "Using source address", "srcIP", srcIP)
	}

	level.Info(logger).Log("msg", "Creating socket")
	if ip.IP.To4() == nil {
		requestType = ipv6.ICMPTypeEchoRequest
		replyType = ipv6.ICMPTypeEchoReply

		if srcIP == nil {
			srcIP = net.ParseIP("::")
		}
		icmpConn, err := icmp.ListenPacket("ip6:ipv6-icmp", srcIP.String())
		if err != nil {
			level.Error(logger).Log("msg", "Error listening to socket", "err", err)
			return
		}

		socket = icmpConn
	} else {
		requestType = ipv4.ICMPTypeEcho
		replyType = ipv4.ICMPTypeEchoReply

		if srcIP == nil {
			srcIP = net.ParseIP("0.0.0.0")
		}
		icmpConn, err := net.ListenPacket("ip4:icmp", srcIP.String())
		if err != nil {
			level.Error(logger).Log("msg", "Error listening to socket", "err", err)
			return
		}

		if module.ICMP.DontFragment {
			rc, err := ipv4.NewRawConn(icmpConn)
			if err != nil {
				level.Error(logger).Log("msg", "Error creating raw connection", "err", err)
				return
			}
			socket = &v4Conn{c: rc, df: true}
		} else {
			socket = icmpConn
		}
	}

	defer socket.Close()

	var data []byte
	if module.ICMP.PayloadSize != 0 {
		data = make([]byte, module.ICMP.PayloadSize)
		copy(data, "Prometheus PingMesh Agent")
	} else {
		data = []byte("Prometheus PingMesh Agent")
	}

	body := &icmp.Echo{
		ID:   icmpID,
		Seq:  int(getICMPSequence()),
		Data: data,
	}
	level.Info(logger).Log("msg", "Creating ICMP packet", "seq", body.Seq, "id", body.ID)
	wm := icmp.Message{
		Type: requestType,
		Code: 0,
		Body: body,
	}

	wb, err := wm.Marshal(nil)
	if err != nil {
		level.Error(logger).Log("msg", "Error marshalling packet", "err", err)
		return
	}

	level.Info(logger).Log("msg", "Writing out packet")
	rttStart := time.Now()
	if _, err = socket.WriteTo(wb, ip); err != nil {
		level.Warn(logger).Log("msg", "Error writing to socket", "err", err)
		return
	}

	// Reply should be the same except for the message type.
	wm.Type = replyType
	wb, err = wm.Marshal(nil)
	if err != nil {
		level.Error(logger).Log("msg", "Error marshalling packet", "err", err)
		return
	}

	rb := make([]byte, 1024)
	deadline, _ := ctx.Deadline()
	if err := socket.SetReadDeadline(deadline); err != nil {
		level.Error(logger).Log("msg", "Error setting socket deadline", "err", err)
		return
	}
	level.Info(logger).Log("msg", "Waiting for reply packets")
	for {
		n, peer, err := socket.ReadFrom(rb)
		if err != nil {
			if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
				level.Warn(logger).Log("msg", "Timeout reading from socket", "target_addr", addr, "err", err)
				return
			}
			level.Error(logger).Log("msg", "Error reading from socket", "err", err)
			continue
		}
		if peer.String() != ip.String() {
			continue
		}
		if replyType == ipv6.ICMPTypeEchoReply {
			// Clear checksum to make comparison succeed.
			rb[2] = 0
			rb[3] = 0
		}
		if bytes.Equal(rb[:n], wb) {
			rtt = time.Since(rttStart).Seconds() * 1000.0
			success = true

			level.Info(logger).Log("msg", "Found matching reply packet")

			return
		}
	}
}

func pingRate(count int, d time.Duration, burst int) *rate.Limiter {
	r := float64(count) / (d.Seconds() * 0.9)
	return rate.NewLimiter(rate.Limit(r), burst)
}

func getIpv4() (ipv4 string) {
	ipv4 = "0.0.0.0"

	ifaces, err := net.Interfaces()
	if err != nil {
		return
	}

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			if ip == nil || ip.IsLoopback() {
				continue
			}
			tempIpv4 := ip.To4()
			if tempIpv4 == nil {
				continue
			}

			ipv4 = tempIpv4.String()

			return
		}
	}
	return
}

func pingChooseProtocol(ctx context.Context, IPProtocol string, fallbackIPProtocol bool, target string, logger log.Logger) (ip *net.IPAddr, lookupTime float64, err error) {

	if IPProtocol == "ip6" || IPProtocol == "" {
		IPProtocol = "ip6"
	} else {
		IPProtocol = "ip4"
	}

	level.Info(logger).Log("msg", "Resolving target address", "ip_protocol", IPProtocol)
	resolveStart := time.Now()

	defer func() {
		lookupTime = time.Since(resolveStart).Seconds()
	}()

	resolver := &net.Resolver{}
	ips, err := resolver.LookupIPAddr(ctx, target)
	if err != nil {
		level.Error(logger).Log("msg", "Resolution with IP protocol failed", "err", err)
		return nil, 0.0, err
	}

	// Return the IP in the requested protocol.
	var fallback *net.IPAddr
	for _, ip := range ips {
		switch IPProtocol {
		case "ip4":
			if ip.IP.To4() != nil {
				level.Info(logger).Log("msg", "Resolved target address", "ip", ip.String())
				return &ip, lookupTime, nil
			}

			// ip4 as fallback
			fallback = &ip

		case "ip6":
			if ip.IP.To4() == nil {
				level.Info(logger).Log("msg", "Resolved target address", "ip", ip.String())
				return &ip, lookupTime, nil
			}

			// ip6 as fallback
			fallback = &ip
		}
	}

	// Unable to find ip and no fallback set.
	if fallback == nil || !fallbackIPProtocol {
		return nil, 0.0, fmt.Errorf("unable to find ip; no fallback")
	}

	level.Info(logger).Log("msg", "Resolved target address", "ip", fallback.String())
	return fallback, lookupTime, nil
}
