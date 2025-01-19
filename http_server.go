// Copyright ©2024 Dan Kortschak. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build http || !bluetooth

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/soypat/seqs/stacks"

	"github.com/kortschak/desk/wifi"
)

var useHTTP = true

func (m *mitm) httpServer(ctx context.Context) error {
	_, stack, err := wifi.SetupWithDHCP(m.dev, wifi.SetupConfig{
		Hostname: "desk",
		TCPPorts: 1,
	}, m.log)
	if err != nil {
		return fmt.Errorf("failed to set up dhcp: %w", err)
	}

	const tcpBufLen = 2048 // Half a page each direction.
	ln, err := stacks.NewTCPListener(stack, stacks.TCPListenerConfig{
		MaxConnections: 3,
		ConnTxBufSize:  tcpBufLen,
		ConnRxBufSize:  tcpBufLen,
	})
	if err != nil {
		return fmt.Errorf("failed to create listener: %w", err)
	}
	const port = 80
	err = ln.StartListening(port)
	if err != nil {
		return fmt.Errorf("failed to start listener: %w", err)
	}

	addr := netip.AddrPortFrom(stack.Addr(), port)
	m.log.LogAttrs(ctx, slog.LevelInfo, "listening", slog.String("addr", "http://"+addr.String()))
	mux := http.NewServeMux()
	mux.Handle("/height/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		m.log.LogAttrs(ctx, slog.LevelInfo, "height report request")
		w.Header().Set("Connection", "close")
		p := m.position.Load().(position)
		if p.mantissa == 0 {
			w.Write([]byte("none"))
			return
		}
		fmt.Fprintf(w, "h=%s", p)
	}))
	mux.Handle("/move_to/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.button.Get() {
			return
		}
		m.log.LogAttrs(ctx, slog.LevelInfo, "set height request")
		w.Header().Set("Connection", "close")
		h, err := strconv.Atoi(r.URL.Query().Get("position"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, err)
			return
		}

		m.log.LogAttrs(ctx, slog.LevelInfo, "request move to stored height", slog.Int("h", h))
		if h < 1 || 4 < h {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, "invalid height: %d", h)
			return
		}

		b := byte(1 << h)
		pkt := []byte{0xa5, 0x00, b, 0xff - b, 0xff}
		m.log.LogAttrs(ctx, slog.LevelInfo, "write pkt to controller", slog.Any("pkt", bytesAttr(pkt)))
		m.act.High()
		time.Sleep(time.Millisecond)
		for range 5 {
			_, err = m.controller.Write(pkt)
			time.Sleep(10 * time.Millisecond)
			if err != nil {
				m.log.Error("write to controller", slog.Any("err", err))
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintf(w, "internal error: %v", err)
				return
			}
		}
		m.alive()
		m.act.Low()
		w.Write([]byte("ok"))
	}))
	mux.Handle("/log_at/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		m.log.LogAttrs(ctx, slog.LevelInfo, "set log level request")
		w.Header().Set("Connection", "close")
		err := m.level.UnmarshalText([]byte(r.URL.Query().Get("level")))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, err)
			return
		}
		m.log.LogAttrs(ctx, slog.LevelInfo, "request level", slog.Any("level", m.level.Level()))
		w.Write([]byte("ok"))
	}))
	mux.Handle("/log/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		m.log.LogAttrs(ctx, slog.LevelInfo, "get log")
		w.Header().Set("Connection", "Keep-Alive")
		w.Header().Set("Transfer-Encoding", "chunked")
		m.sw.use(w)
		defer m.sw.close()
		time.Sleep(10 * time.Minute)
	}))
	mux.Handle("/bt/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		m.log.LogAttrs(ctx, slog.LevelInfo, "set bluetooth state")
		w.Header().Set("Connection", "close")
		switch allow := r.URL.Query().Get("allow"); allow {
		case "true":
			m.bluetoothBlocked.Store(false)
		case "false":
			m.bluetoothBlocked.Store(true)
		default:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, "unknown state: %q", allow)
			return
		}
		m.log.LogAttrs(ctx, slog.LevelInfo, "set bluetooth state", slog.Bool("allow", m.bluetoothBlocked.Load()))
		w.Write([]byte("ok"))
	}))
	return http.Serve(ln, mux)
}
