// Copyright ©2025 Dan Kortschak. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !bluetooth

package main

import "context"

var useBluetooth = false

func (m *mitm) bluetoothServer(context.Context) error { return nil }
