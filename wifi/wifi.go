// MIT License
//
// Copyright (c) 2022 Patricio Whittingslow
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package wifi

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"time"

	_ "embed"

	"github.com/soypat/cyw43439"
	"github.com/soypat/seqs/eth/dhcp"
	"github.com/soypat/seqs/eth/dns"
	"github.com/soypat/seqs/stacks"
)

var (
	//go:embed ssid.text
	ssid string
	//go:embed password.text
	pass string
)

const mtu = cyw43439.MTU

type SetupConfig struct {
	// DHCP requested hostname.
	Hostname string
	// DHCP requested IP address. On failing to find DHCP server is used as static IP.
	RequestedIP string
	// Number of UDP ports to open for the stack. (we'll actually open one more than this for DHCP)
	UDPPorts uint16
	// Number of TCP ports to open for the stack.
	TCPPorts uint16
}

var nolog = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
	Level: slog.Level(127), // Make temporary logger that does no logging.
}))

func SetupWithDHCP(dev *cyw43439.Device, cfg SetupConfig, log *slog.Logger) (*stacks.DHCPClient, *stacks.PortStack, error) {
	cfg.UDPPorts++ // Add extra UDP port for DHCP client.
	if log == nil {
		log = nolog
	}
	var (
		addr netip.Addr
		err  error
	)
	if cfg.RequestedIP != "" {
		addr, err = netip.ParseAddr(cfg.RequestedIP)
		if err != nil {
			return nil, nil, err
		}
	}

	if pass == "" {
		log.Info("joining open network:", slog.String("ssid", ssid))
	} else {
		log.Info("joining WPA secure network", slog.String("ssid", ssid), slog.Int("passlen", len(pass)))
	}
	for {
		err = dev.JoinWPA2(ssid, pass)
		if err == nil {
			break
		}
		log.Error("failed to join wifi", slog.Any("err", err))
		time.Sleep(5 * time.Second)
	}
	mac, err := dev.HardwareAddr6()
	if err != nil {
		return nil, nil, err
	}
	log.Info("wifi join success!", slog.String("mac", net.HardwareAddr(mac[:]).String()))

	stack := stacks.NewPortStack(stacks.PortStackConfig{
		MAC:             mac,
		MaxOpenPortsUDP: int(cfg.UDPPorts),
		MaxOpenPortsTCP: int(cfg.TCPPorts),
		MTU:             mtu,
		Logger:          log,
	})

	dev.RecvEthHandle(stack.RecvEth)

	// Begin asynchronous packet handling.
	go nicLoop(dev, stack)

	// Perform DHCP request.
	dhcpClient := stacks.NewDHCPClient(stack, dhcp.DefaultClientPort)
	err = dhcpClient.BeginRequest(stacks.DHCPRequestConfig{
		RequestedAddr: addr,
		Xid:           uint32(time.Now().Nanosecond()),
		Hostname:      cfg.Hostname,
	})
	if err != nil {
		return nil, stack, fmt.Errorf("dhcp begin request: %w", err)
	}
	i := 0
	for dhcpClient.State() != dhcp.StateBound {
		i++
		log.Info("DHCP ongoing...")
		time.Sleep(time.Second / 2)
		if i > 15 {
			if !addr.IsValid() {
				return dhcpClient, stack, errors.New("DHCP did not complete and no static IP was requested")
			}
			log.Info("DHCP did not complete, assigning static IP", slog.String("ip", cfg.RequestedIP))
			stack.SetAddr(addr)
			return dhcpClient, stack, nil
		}
	}

	var primaryDNS netip.Addr
	if srv := dhcpClient.DNSServers(); len(srv) > 0 {
		primaryDNS = srv[0]
	}
	ip := dhcpClient.Offer()
	log.Info("DHCP complete",
		slog.Uint64("cidrbits", uint64(dhcpClient.CIDRBits())),
		slog.String("ourIP", ip.String()),
		slog.String("dns", primaryDNS.String()),
		slog.String("broadcast", dhcpClient.BroadcastAddr().String()),
		slog.String("gateway", dhcpClient.Gateway().String()),
		slog.String("router", dhcpClient.Router().String()),
		slog.String("dhcp", dhcpClient.DHCPServer().String()),
		slog.String("hostname", string(dhcpClient.Hostname())),
		slog.Duration("lease", dhcpClient.IPLeaseTime()),
		slog.Duration("renewal", dhcpClient.RenewalTime()),
		slog.Duration("rebinding", dhcpClient.RebindingTime()),
	)
	stack.SetAddr(ip) // It's important to set the IP address after DHCP completes.

	return dhcpClient, stack, nil
}

// ResolveHardwareAddr obtains the hardware address of the given IP address.
func ResolveHardwareAddr(stack *stacks.PortStack, ip netip.Addr) ([6]byte, error) {
	if !ip.IsValid() {
		return [6]byte{}, errors.New("invalid ip")
	}
	arpc := stack.ARP()
	arpc.Abort() // Remove any previous ARP requests.
	err := arpc.BeginResolve(ip)
	if err != nil {
		return [6]byte{}, err
	}
	time.Sleep(4 * time.Millisecond)
	// ARP exchanges should be fast, don't wait too long for them.
	const timeout = time.Second
	const maxretries = 20
	retries := maxretries
	for !arpc.IsDone() && retries > 0 {
		retries--
		if retries == 0 {
			return [6]byte{}, errors.New("arp timed out")
		}
		time.Sleep(timeout / maxretries)
	}
	_, hw, err := arpc.ResultAs6()
	return hw, err
}

type Resolver struct {
	stack     *stacks.PortStack
	dns       *stacks.DNSClient
	dhcp      *stacks.DHCPClient
	dnsaddr   netip.Addr
	dnshwaddr [6]byte
}

func NewResolver(stack *stacks.PortStack, dhcp *stacks.DHCPClient) (*Resolver, error) {
	dnsc := stacks.NewDNSClient(stack, dns.ClientPort)
	dnsaddrs := dhcp.DNSServers()
	if len(dnsaddrs) > 0 && !dnsaddrs[0].IsValid() {
		return nil, errors.New("dns addr obtained via DHCP not valid")
	}
	return &Resolver{
		stack:   stack,
		dhcp:    dhcp,
		dns:     dnsc,
		dnsaddr: dnsaddrs[0],
	}, nil
}

func (r *Resolver) LookupNetIP(host string) ([]netip.Addr, error) {
	name, err := dns.NewName(host)
	if err != nil {
		return nil, err
	}
	err = r.updateDNSHWAddr()
	if err != nil {
		return nil, err
	}

	err = r.dns.StartResolve(r.dnsConfig(name))
	if err != nil {
		return nil, err
	}
	time.Sleep(5 * time.Millisecond)
	retries := 100

	for retries > 0 {
		done, _ := r.dns.IsDone()
		if done {
			break
		}
		retries--
		time.Sleep(20 * time.Millisecond)
	}
	done, rcode := r.dns.IsDone()
	if !done && retries == 0 {
		return nil, errors.New("dns lookup timed out")
	} else if rcode != dns.RCodeSuccess {
		return nil, errors.New("dns lookup failed:" + rcode.String())
	}
	answers := r.dns.Answers()
	if len(answers) == 0 {
		return nil, errors.New("no dns answers")
	}
	var addrs []netip.Addr
	for i := range answers {
		data := answers[i].RawData()
		if len(data) == 4 {
			addrs = append(addrs, netip.AddrFrom4([4]byte(data)))
		}
	}
	if len(addrs) == 0 {
		return nil, errors.New("no ipv4 dns answers")
	}
	return addrs, nil
}

func (r *Resolver) updateDNSHWAddr() (err error) {
	r.dnshwaddr, err = ResolveHardwareAddr(r.stack, r.dnsaddr)
	return err
}

func (r *Resolver) dnsConfig(name dns.Name) stacks.DNSResolveConfig {
	return stacks.DNSResolveConfig{
		Questions: []dns.Question{
			{
				Name:  name,
				Type:  dns.TypeA,
				Class: dns.ClassINET,
			},
		},
		DNSAddr:         r.dnsaddr,
		DNSHWAddr:       r.dnshwaddr,
		EnableRecursion: true,
	}
}

func nicLoop(dev *cyw43439.Device, Stack *stacks.PortStack) {
	// Maximum number of packets to queue before sending them.
	const (
		queueSize                = 3
		maxRetriesBeforeDropping = 3
	)
	var queue [queueSize][mtu]byte
	var lenBuf [queueSize]int
	var retries [queueSize]int
	markSent := func(i int) {
		queue[i] = [mtu]byte{} // Not really necessary.
		lenBuf[i] = 0
		retries[i] = 0
	}
	for {
		stallRx := true
		// Poll for incoming packets.
		for i := 0; i < 1; i++ {
			gotPacket, err := dev.PollOne()
			if err != nil {
				println("poll error:", err.Error())
			}
			if !gotPacket {
				break
			}
			stallRx = false
		}

		// Queue packets to be sent.
		for i := range queue {
			if retries[i] != 0 {
				continue // Packet currently queued for retransmission.
			}
			var err error
			buf := queue[i][:]
			lenBuf[i], err = Stack.HandleEth(buf[:])
			if err != nil {
				println("stack error n(should be 0)=", lenBuf[i], "err=", err.Error())
				lenBuf[i] = 0
				continue
			}
			if lenBuf[i] == 0 {
				break
			}
		}
		stallTx := lenBuf == [queueSize]int{}
		if stallTx {
			if stallRx {
				// Avoid busy waiting when both Rx and Tx stall.
				time.Sleep(51 * time.Millisecond)
			}
			continue
		}

		// Send queued packets.
		for i := range queue {
			n := lenBuf[i]
			if n <= 0 {
				continue
			}
			err := dev.SendEth(queue[i][:n])
			if err != nil {
				// Queue packet for retransmission.
				retries[i]++
				if retries[i] > maxRetriesBeforeDropping {
					markSent(i)
					println("dropped outgoing packet:", err.Error())
				}
			} else {
				markSent(i)
			}
		}
	}
}
