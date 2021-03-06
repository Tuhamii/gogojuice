// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sys

const (
	TheChar       = '6'
	BigEndian     = 0
	CacheLineSize = 64
	PhysPageSize  = 4096
	PCQuantum     = 1
	Int64Align    = 8
	HugePageSize  = 1 << 21
	MinFrameSize  = 0
)

type Uintreg uint64
type Intptr int64 // TODO(rsc): remove
