// Copyright ©2024 Dan Kortschak. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"math/bits"
	"time"

	"github.com/soypat/cyw43439"
)

// ledError is an error with an associated LED flash sequence.
type ledError struct {
	error
	seq ledSequence
}

// newLedError returns a ledError with a flash sequence defined by n, which
// should be program-unique. Uniqueness is not checked.
func newLedError(n byte, err error) ledError {
	return ledError{error: err, seq: errorSequence(n)}
}

func (e ledError) ledSequence() ledSequence { return e.seq }

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
