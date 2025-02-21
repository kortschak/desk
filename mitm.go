// Copyright ©2024 Dan Kortschak. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"log/slog"
	"machine"
	"sync"
	"sync/atomic"
	"time"

	"github.com/soypat/cyw43439"
)

type mitm struct {
	dev *cyw43439.Device

	handset *machine.UART
	button  machine.Pin

	mu         sync.Mutex
	controller *machine.UART
	act        machine.Pin
	last       chan time.Time

	position         atomic.Value // position
	bluetoothBlocked atomic.Bool

	log   *slog.Logger
	sw    switchedWriter
	level slog.LevelVar
}

func (m *mitm) init(ctx context.Context) error {
	m.log.LogAttrs(ctx, slog.LevelInfo, "configure pico W device")
	start := time.Now()

	var cfg cyw43439.Config
	if useBluetooth {
		cfg = cyw43439.DefaultWifiBluetoothConfig()
	} else {
		cfg = cyw43439.DefaultWifiConfig()
	}
	err := m.dev.Init(cfg)
	if err != nil {
		return newLedError(1, err)
	}
	m.log.LogAttrs(ctx, slog.LevelInfo, "cyw43439 initialised", slog.Duration("duration", time.Since(start)))

	m.log.LogAttrs(ctx, slog.LevelInfo, "configure pins")
	m.button.Configure(machine.PinConfig{
		Mode: machine.PinInputPulldown,
	})
	m.act.Configure(machine.PinConfig{
		Mode: machine.PinOutput,
	})

	m.log.LogAttrs(ctx, slog.LevelInfo, "configure uarts")
	m.log.LogAttrs(ctx, slog.LevelInfo, "configure controller uart")
	err = m.controller.Configure(machine.UARTConfig{
		BaudRate: 9600,
		TX:       machine.UART1_TX_PIN, // P11
		RX:       machine.UART1_RX_PIN, // P12
	})
	if err != nil {
		return newLedError(2, err)
	}
	m.log.LogAttrs(ctx, slog.LevelInfo, "configure handset uart")
	err = m.handset.Configure(machine.UARTConfig{
		BaudRate: 9600,
		TX:       machine.UART0_TX_PIN, // P1
		RX:       machine.UART0_RX_PIN, // P2
	})
	if err != nil {
		return newLedError(3, err)
	}

	m.log.LogAttrs(ctx, slog.LevelInfo, "set up watchdog")
	machine.Watchdog.Configure(machine.WatchdogConfig{
		TimeoutMillis: 10000,
	})
	err = machine.Watchdog.Start()
	if err != nil {
		return newLedError(4, err)
	}

	m.log.LogAttrs(ctx, slog.LevelInfo, "read uart")
	const poll = 10 * time.Millisecond
	var lastP string // Read and write only in the following goroutine.
	go m.readUART(ctx, "handset", 0xa5, 5, m.handset, poll, func(pkt []byte) {
		machine.Watchdog.Update()
		p, err := key(pkt[1:])
		if err != nil {
			if err != errReset {
				m.log.LogAttrs(ctx, slog.LevelError, "key", slog.Any("err", err), slog.Any("pkt", bytesAttr(pkt)))
				return
			}
			m.log.LogAttrs(ctx, slog.LevelInfo-1, "key", slog.Any("err", err), slog.Any("pkt", bytesAttr(pkt)))
		}
		if p != lastP {
			m.log.LogAttrs(ctx, slog.LevelInfo-1, "key", slog.String("press", p))
			lastP = p
		}
		if !m.mu.TryLock() {
			return
		}
		defer m.mu.Unlock()
		_, err = m.controller.Write(pkt)
		time.Sleep(poll)
		if err != nil {
			m.log.LogAttrs(ctx, slog.LevelError, "write handset uart", slog.Any("err", err))
		}
	})
	go m.readUART(ctx, "controller", 0x5a, 5, m.controller, poll, func(pkt []byte) {
		machine.Watchdog.Update()
		p, err := height(pkt[1:])
		if err != nil && err != errNoHeight {
			m.log.LogAttrs(ctx, slog.LevelError, "height", slog.Any("err", err), slog.Any("pkt", bytesAttr(pkt)))
			return
		}
		if err != errNoHeight {
			m.log.LogAttrs(ctx, slog.LevelInfo-1, "height", slog.Any("position", p), slog.Any("pkt", bytesAttr(pkt)))
			m.position.Store(p)
		}
	})

	return nil
}

const keepAliveInterval = 15 * time.Minute

func (m *mitm) keepAlive(ctx context.Context) {
	pkt := []byte{0xa5, 0x00, 0x60, 0x9f, 0xff} // Packet is an Up+Down button press.
	last := time.Now()
	for {
		// TODO: Replace this with the commented case below and remove
		// the timer when tinygo supports go1.23 time.Timer behaviour.
		timer := time.NewTimer(last.Add(keepAliveInterval).Sub(time.Now()))

		select {
		// case last = <-time.After(last.Add(keepAliveInterval).Sub(time.Now())):
		case last = <-timer.C:
			m.log.LogAttrs(ctx, slog.LevelInfo, "send keep-alive")
			func() {
				m.mu.Lock()
				defer m.mu.Unlock()

				m.log.LogAttrs(ctx, slog.LevelDebug, "write keep-alive pkt to controller", slog.Any("pkt", bytesAttr(pkt)))
				m.act.High()
				time.Sleep(time.Millisecond)
				for range 5 {
					_, err := m.controller.Write(pkt)
					time.Sleep(10 * time.Millisecond)
					if err != nil {
						m.log.Error("write to controller", slog.Any("err", err))
						return
					}
				}
				m.act.Low()
			}()
		case last = <-m.last:
			if !timer.Stop() {
				<-timer.C
			}
			m.log.LogAttrs(ctx, slog.LevelDebug, "delay keep-alive", slog.Any("until", last.Add(keepAliveInterval)))
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		}
	}
}

func (m *mitm) alive() {
	select {
	case m.last <- time.Now():
	default:
	}
}

func (m *mitm) readUART(ctx context.Context, name string, start byte, len int, uart *machine.UART, wait time.Duration, do func([]byte)) {
	r := uartReader{
		src:   uart,
		wait:  wait,
		start: start,
		len:   len,
	}
	defer m.log.LogAttrs(ctx, slog.LevelInfo, "exit read uart")
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		pkt, err := r.packet(ctx)
		if err == context.Canceled {
			return
		}
		if err != nil {
			m.log.LogAttrs(ctx, slog.LevelError, "read", slog.String("name", name), slog.Any("pkt", bytesAttr(pkt)), slog.Any("err", err))
			continue
		}
		m.log.LogAttrs(ctx, slog.LevelDebug, "read", slog.String("name", name), slog.Any("pkt", bytesAttr(pkt)))

		do(pkt)
	}
}
