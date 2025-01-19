// Copyright ©2025 Dan Kortschak. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build bluetooth

package main

import (
	"context"
	"log/slog"
	"strings"
	"time"

	_ "embed"

	"tinygo.org/x/bluetooth"
)

var useBluetooth = true

var (
	//go:embed advertise_name.text
	name string
	//go:embed service.uuid
	service string
	//go:embed move_to.uuid
	moveTo string
	//go:embed height.uuid
	getHeight string
)

func (m *mitm) bluetoothServer(ctx context.Context) error {
	serviceUUID, err := bluetooth.ParseUUID(strings.TrimSpace(service))
	if err != nil {
		return err
	}
	moveToUUID, err := bluetooth.ParseUUID(strings.TrimSpace(moveTo))
	if err != nil {
		return err
	}
	heightUUID, err := bluetooth.ParseUUID(strings.TrimSpace(getHeight))
	if err != nil {
		return err
	}

	adapter := bluetooth.DefaultAdapter
	adapter.Use(m.dev)

	adv := adapter.DefaultAdvertisement()
	err = adv.Configure(bluetooth.AdvertisementOptions{
		LocalName: strings.TrimSpace(name),
	})
	if err != nil {
		return err
	}
	err = adv.Start()
	if err != nil {
		return err
	}
	var (
		pos     bluetooth.Characteristic
		posData [1]byte

		high     bluetooth.Characteristic
		highData [4]byte
	)
	return adapter.AddService(&bluetooth.Service{
		UUID: serviceUUID,
		Characteristics: []bluetooth.CharacteristicConfig{
			{
				Handle: &pos,
				UUID:   moveToUUID,
				Value:  posData[:],
				Flags:  bluetooth.CharacteristicReadPermission | bluetooth.CharacteristicWritePermission | bluetooth.CharacteristicWriteWithoutResponsePermission,
				WriteEvent: func(client bluetooth.Connection, offset int, value []byte) {
					if m.bluetoothBlocked.Load() {
						m.log.LogAttrs(ctx, slog.LevelError, "bluetooth control disabled")
						return
					}
					if offset != 0 || len(value) != 1 {
						return
					}
					m.mu.Lock()
					defer m.mu.Unlock()
					if m.button.Get() {
						return
					}
					m.log.LogAttrs(ctx, slog.LevelInfo, "set height request")
					h := int(value[0])
					if h < 1 || 4 < h {
						m.log.LogAttrs(ctx, slog.LevelError, "invalid height value", slog.Int("h", h))
						return
					}
					m.log.LogAttrs(ctx, slog.LevelInfo, "request move to stored height", slog.Int("h", h))

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
							return
						}
					}
					m.alive()
					m.act.Low()

					posData[0] = value[0]
				},
			},

			{
				Handle: &high,
				UUID:   heightUUID,
				Value:  highData[:],
				Flags:  bluetooth.CharacteristicReadPermission,
				ReadEvent: func(client bluetooth.Connection, offset int, value []byte) {
					if m.bluetoothBlocked.Load() {
						m.log.LogAttrs(ctx, slog.LevelError, "bluetooth control disabled")
						return
					}
					if offset != 0 || len(value) != 4 {
						return
					}
					m.log.LogAttrs(ctx, slog.LevelInfo, "height report request")
					clear(value)
					copy(value, m.position.Load().(position).String())
				},
			},
		},
	})
}
