// Copyright ©2024 Dan Kortschak. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"machine"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/soypat/cyw43439"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Let serial port stabilise.
	time.Sleep(time.Second)

	m := mitm{
		dev: cyw43439.NewPicoWDevice(),

		handset: machine.UART0,
		button:  machine.GPIO15, // P20

		controller: machine.UART1,
		act:        machine.GPIO16, // P21
	}
	m.position.Store(position{})
	m.level.Set(slog.LevelInfo)
	m.log = slog.New(slog.NewTextHandler(
		io.MultiWriter(machine.Serial, &m.sw),
		&slog.HandlerOptions{
			Level: &m.level,
		},
	))
	m.log.LogAttrs(ctx, slog.LevelInfo, "initialise pico W device")

	defer func() {
		cancel()
		r := recover()
		switch r := r.(type) {
		case nil:
		case ledSequencer:
			m.log.LogAttrs(ctx, slog.LevelError, "flatline", slog.Any("err", r))
			for {
				machine.Watchdog.Update()
				err := flash(m.dev, r.ledSequence())
				if err != nil {
					m.log.LogAttrs(ctx, slog.LevelError, "flatline flash", slog.Any("err", err))
				}
			}
		default:
			m.log.LogAttrs(ctx, slog.LevelError, "flatline", slog.Any("err", r))
			for {
				machine.Watchdog.Update()
				err := flash(m.dev, uncaughtPanic)
				if err != nil {
					m.log.LogAttrs(ctx, slog.LevelError, "flatline flash", slog.Any("err", err))
				}
			}
		}
	}()

	err := m.init(ctx)
	if err != nil {
		panic(err)
	}

	m.log.LogAttrs(ctx, slog.LevelInfo, "pass through pin")
	m.button.SetInterrupt(machine.PinToggle, func(pin machine.Pin) {
		m.act.Set(pin.Get())
	})

	m.log.LogAttrs(ctx, slog.LevelInfo, "start server")
	go func() {
		err := m.server(ctx)
		if err != nil {
			panic(err)
		}
	}()

	m.log.LogAttrs(ctx, slog.LevelInfo, "start heartbeat")
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		machine.Watchdog.Update()
		err := flash(m.dev, normalOperation)
		if err != nil {
			m.log.LogAttrs(ctx, slog.LevelError, "heartbeat", slog.Any("err", err))
		}
	}
}

type switchedWriter struct {
	cw atomic.Pointer[io.Writer]
}

func (w *switchedWriter) Write(p []byte) (int, error) {
	cw := w.cw.Load()
	if cw == nil {
		return len(p), nil
	}
	n, err := (*cw).Write(p)
	if w, ok := (*cw).(http.Flusher); ok {
		w.Flush()
	}
	return n, err
}

func (w *switchedWriter) use(val io.Writer) {
	w.cw.Store(&val)
}

func (w *switchedWriter) close() {
	w.cw.Store(nil)
}

type bytesAttr []byte

func (b bytesAttr) LogValue() slog.Value {
	return slog.StringValue(fmt.Sprintf("%x", []byte(b)))
}
