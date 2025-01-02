// Copyright ©2024 Dan Kortschak. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"machine"
	"slices"
	"strconv"
	"strings"
	"time"
)

// uartReader is a UART packet reader.
type uartReader struct {
	src  *machine.UART
	buf  [16]byte
	wait time.Duration

	start byte
	len   int
	read  []byte
	pkt   []byte
}

// packet returns the next packet.
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

// nextPacket fills dst with an n-packet from src and returns the packet and
// remaining data.
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

var (
	errNoHeight = errors.New("height value is empty")
	errExtraDot = errors.New("unexpected decimal point")

	errReset = errors.New("reset")

	errInvalidPacketLength = errors.New("invalid packet length")
	errChecksumMismatch    = errors.New("checksum mismatch")

	errShortPacket = errors.New("packet too short")
	errLongPacket  = errors.New("packet too long")
)

// contErr is a controller error state.
type contErr byte

// newContError returns the controller error encoded in p, or nil if p is not an
// error state.
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

// key returns the set of buttons that are marked as pressed in the provided
// packet.
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

// position is a desk height position.
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

	// Error/status codes.
	0b01110111: 'R', // 0x77
	0b01111000: 'T', // 0x78
	0b01111001: 'E', // 0x79 should be 0b01111100
}
