package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"machine"
	"math/bits"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/soypat/cyw43439"
	"github.com/soypat/seqs/stacks"

	"github.com/kortschak/desk/wifi"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	machine.UART1_TX_PIN.Configure(machine.PinConfig{
		Mode: machine.PinOutput,
	})
	machine.UART1_TX_PIN.Low()
	machine.UART0_TX_PIN.Configure(machine.PinConfig{
		Mode: machine.PinOutput,
	})
	machine.UART0_TX_PIN.Low()

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

type mitm struct {
	dev *cyw43439.Device

	handset *machine.UART
	button  machine.Pin

	mu         sync.Mutex
	controller *machine.UART
	act        machine.Pin

	position atomic.Value // position

	log   *slog.Logger
	sw    switchedWriter
	level slog.LevelVar
}

func (m *mitm) init(ctx context.Context) error {
	m.log.LogAttrs(ctx, slog.LevelInfo, "configure pico W device")
	start := time.Now()
	err := m.dev.Init(cyw43439.DefaultWifiConfig())
	if err != nil {
		return err
	}
	m.log.LogAttrs(ctx, slog.LevelInfo, "cyw43439 initialised", slog.Duration("duration", time.Since(start)))

	m.log.LogAttrs(ctx, slog.LevelInfo, "configure pins")
	m.button.Configure(machine.PinConfig{
		Mode: machine.PinInputPulldown,
	})
	m.act.Configure(machine.PinConfig{
		Mode: machine.PinOutput,
	})
	m.act.High()

	m.log.LogAttrs(ctx, slog.LevelInfo, "configure uarts")
	m.log.LogAttrs(ctx, slog.LevelInfo, "configure controller UART")
	err = m.controller.Configure(machine.UARTConfig{
		BaudRate: 9600,
		TX:       machine.UART1_TX_PIN, // P11
		RX:       machine.UART1_RX_PIN, // P12
	})
	if err != nil {
		return err
	}
	m.log.LogAttrs(ctx, slog.LevelInfo, "configure handset UART")
	err = m.handset.Configure(machine.UARTConfig{
		BaudRate: 9600,
		TX:       machine.UART0_TX_PIN, // P1
		RX:       machine.UART0_RX_PIN, // P2
	})
	if err != nil {
		return err
	}

	m.log.LogAttrs(ctx, slog.LevelInfo, "trigger presence")
	time.Sleep(16 * time.Millisecond)
	m.act.Low()

	m.log.LogAttrs(ctx, slog.LevelInfo, "set up watchdog")
	machine.Watchdog.Configure(machine.WatchdogConfig{
		TimeoutMillis: 10000,
	})
	err = machine.Watchdog.Start()
	if err != nil {
		return err
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
			m.log.LogAttrs(ctx, slog.LevelError, "write uart1", slog.Any("err", err))
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
		_, err = m.handset.Write(pkt)
		time.Sleep(poll)
		if err != nil {
			m.log.LogAttrs(ctx, slog.LevelError, "write uart0", slog.Any("err", err))
		}
	})

	for range 10 {
		machine.Watchdog.Update()
		m.log.LogAttrs(ctx, slog.LevelInfo, "write")
		m.mu.Lock()
		_, err := m.controller.Write([]byte{0xa5, 0x0, 0x0, 0xff, 0xff})
		m.mu.Unlock()
		if err != nil {
			m.log.LogAttrs(ctx, slog.LevelError, "write to controller", slog.Any("err", err))
		}
		time.Sleep(10 * time.Millisecond)
	}

	return nil
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

type uartReader struct {
	src  *machine.UART
	buf  [16]byte
	wait time.Duration

	start byte
	len   int
	read  []byte
	pkt   []byte
}

func (r *uartReader) packet(ctx context.Context) ([]byte, error) {
	if r.pkt == nil {
		r.pkt = make([]byte, r.len)
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if r.src.Buffered() == 0 {
			time.Sleep(r.wait)
			continue
		}

		n, err := r.src.Read(r.buf[:])
		if err != nil {
			b := r.read
			r.read = r.read[:0]
			return b, err
		}
		if n == 0 {
			continue
		}

		if (len(r.read) == 0 && r.buf[0] == r.start) || len(r.read) != 0 {
			r.read = append(r.read, r.buf[:n]...)
		}
		if len(r.read) < r.len {
			continue
		}
		pkt := r.pkt
		pkt, r.read, err = nextPacket(r.pkt, r.read, r.start, r.len)
		return pkt, err
	}
}

func nextPacket(dst, src []byte, delim byte, n int) (pkt, rest []byte, err error) {
	if len(src) < n {
		return nil, src, nil
	}
	next := bytes.IndexByte(src[1:], delim)
	if next < 0 {
		next = len(src)
	} else {
		next++
	}
	if next > n {
		copy(dst, src[:n])
		var check byte
		for _, b := range dst[:n-1] {
			check += b
		}
		if check != dst[n-1] {
			return dst, src[:0], errLongPacket
		}
		return dst, src[:0], nil
	}
	dst = dst[:next]
	copy(dst, src[:next])
	if next < n {
		err = errShortPacket
	}
	return dst, slices.Delete(src, 0, next), err
}

func (m *mitm) server(ctx context.Context) error {
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
	return http.Serve(ln, mux)
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

var (
	errNoHeight = errors.New("height value is empty")
	errExtraDot = errors.New("unexpected decimal point")

	errReset = errors.New("reset")

	errInvalidPacketLength = errors.New("invalid packet length")
	errChecksumMismatch    = errors.New("checksum mismatch")

	errShortPacket = errors.New("packet too short")
	errLongPacket  = errors.New("packet too long")
)

type contErr byte

func newContErr(p []byte) error {
	if len(p) != 4 || p[0] != 0x79 {
		return nil
	}
	var check error
	if p[0]+p[1]+p[2] != p[3] {
		check = errChecksumMismatch
	}
	h, _ := digit(p[1])
	l, _ := digit(p[2])
	if check != nil {
		return errors.Join(contErr(10*(h-'0')+(l-'0')), check)
	}
	return contErr(10*(h-'0') + (l - '0'))
}

func (e contErr) Error() string { return fmt.Sprintf("E%02d", e) }

func key(p []byte) (string, error) {
	if len(p) != 4 {
		return "", errInvalidPacketLength
	}
	var check byte
	for _, b := range p[:3] {
		check += b
	}
	if check != p[3] {
		return "", errChecksumMismatch
	}
	const press = "m1234ud"
	var buf [len(press)]byte
	n := 0
	for i, c := range press {
		if p[1]&(1<<i) != 0 {
			buf[n] = byte(c)
			n++
		}
	}
	if n == 0 {
		return "_", nil
	}
	return string(buf[:n]), nil
}

type position struct {
	mantissa int
	exponent int
}

// height returns the position for the height encoded in p.
func height(p []byte) (position, error) {
	if len(p) != 4 {
		return position{}, errInvalidPacketLength
	}
	if bytes.Equal(p, []byte{0, 0, 0, 0}) {
		return position{}, errNoHeight
	}
	if bytes.Equal(p, []byte{0x77, 0x6d, 0x78, 0x5c}) {
		return position{}, errReset
	}
	err := newContErr(p)
	if err != nil {
		return position{}, err
	}
	var (
		check byte
		mant  int
		dot   = 2
	)
	for i, b := range p[:3] {
		check += b
		d, ok := digit(b)
		if ok {
			if dot != 2 {
				return position{}, errExtraDot
			}
			dot = i
		}
		mant = 10*mant + int(d-'0')
	}
	if check != p[3] {
		return position{mant, dot - 2}, errChecksumMismatch
	}
	return position{mant, dot - 2}, nil
}

// digit returns the digit associated with the provided byte data, and whether
// the decimal point is set for the digit.
func digit(b byte) (digit byte, dot bool) {
	digit = digits[b&^0x80]
	if digit == 0 {
		digit = '?'
	}
	return digit, b&0x80 != 0
}

// digits is the mapping from wire data to digits. The mapping is based on
// the segments of a 7-segment display. Only digits without the decimal point
// are represented.
//
// 7-segment display
//
//	  -2-
//	|     |
//	5     1
//	|     |
//	  -6-
//	|     |
//	4     0
//	|     |
//	  -3-   7
var digits = [256]byte{
	0b00111111: '0', // 0x3f
	0b00000110: '1', // 0x06
	0b01011011: '2', // 0x5b
	0b01001111: '3', // 0x4f
	0b01100110: '4', // 0x66 should be 0b01100011 ¯\_(ツ)_/¯
	0b01101101: '5', // 0x6d
	0b01111101: '6', // 0x7d
	0b00000111: '7', // 0x07
	0b01111111: '8', // 0x7f
	0b01101111: '9', // 0x6f

	// Error codes.
	0b01110111: 'R', // 0x77
	0b01111000: 'T', // 0x78
	0b01111001: 'E', // 0x79 should be 0b01111100 ¯\_(ツ)_/¯
}

func (p position) String() string {
	switch {
	case p.exponent == 0:
		return strconv.Itoa(p.mantissa)
	case p.exponent < 0:
		// This is only valid for cases where the height
		// is at least 1. This is the case for the data
		// we get from the device.
		d := 1
		for range -p.exponent {
			d *= 10
		}
		return fmt.Sprintf("%d.%d", p.mantissa/d, p.mantissa%d)
	default:
		return strconv.Itoa(p.mantissa) + strings.Repeat("0", p.exponent)
	}
}

type ledSequencer interface {
	ledSequence() ledSequence
}

var (
	// normalOperation is the standard operation heartbeat.
	normalOperation = ledSequence{
		{on: true, duration: 10 * time.Millisecond},
		{on: false, duration: 990 * time.Millisecond},
	}
	// uncaughtPanic is the panic termination heartbeat.
	uncaughtPanic = ledSequence{
		{on: true, duration: 990 * time.Millisecond},
		{on: false, duration: 10 * time.Millisecond},
	}
)

// errorSequence returns an ledSequence that encodes n as a set of four counts
// of one to four indicating the numbers four two-bit nyblets in big-endian
// order.
func errorSequence(n byte) ledSequence {
	if n == 0 {
		return ledSequence{
			{on: true, duration: 300 * time.Millisecond},
			{on: false, duration: 2 * time.Second},
		}
	}
	nyblets := bits.LeadingZeros8(n) / 2
	n <<= nyblets * 2
	nyblets = 4 - nyblets
	seq := make(ledSequence, 0, 32)

	for range nyblets {
		nyblet := (n & (0b11 << 6)) >> 6
		for range nyblet + 1 {
			seq = append(seq,
				ledState{on: true, duration: 300 * time.Millisecond},
				ledState{on: false, duration: 250 * time.Millisecond},
			)
		}
		seq[len(seq)-1].duration = 500 * time.Millisecond
		n <<= 2
	}
	seq[len(seq)-1].duration = 2 * time.Second
	return seq
}

// flash flashes the LED sequence in seq on the target device.
func flash(dev *cyw43439.Device, seq ledSequence) error {
	for _, state := range seq {
		err := dev.GPIOSet(0, state.on)
		if err != nil {
			return err
		}
		time.Sleep(state.duration)
	}
	return nil
}

// ledSequence is a sequence of LED states.
type ledSequence []ledState

// ledState represents an LED state over a duration.
type ledState struct {
	on       bool
	duration time.Duration
}

type bytesAttr []byte

func (b bytesAttr) LogValue() slog.Value {
	return slog.StringValue(fmt.Sprintf("%x", []byte(b)))
}
