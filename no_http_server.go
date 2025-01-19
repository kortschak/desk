// Copyright ©2025 Dan Kortschak. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !(http || !bluetooth)

package main

import "context"

var useHTTP = false

func (m *mitm) httpServer(context.Context) error { return nil }
