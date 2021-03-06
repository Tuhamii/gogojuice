// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sys

const (
	TheChar       = '9'
	BigEndian     = 1
	CacheLineSize = 64
	PhysPageSize  = 65536
	PCQuantum     = 4
	Int64Align    = 8
	HugePageSize  = 0
	MinFrameSize  = 8
)

type Uintreg uint64
type Intptr int64 // TODO(rsc): remove
