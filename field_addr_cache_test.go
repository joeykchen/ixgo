/*
 * Copyright (c) 2026 The GoPlus Authors (goplus.org). All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package ixgo_test

import (
	"strings"
	"testing"

	"github.com/goplus/ixgo"
)

func runFieldAddrCacheProgram(t *testing.T, source string, poolThreshold int) {
	t.Helper()
	ctx := ixgo.NewContext(0)
	ctx.SetLeastCallForEnablePool(poolThreshold)
	if _, err := ctx.RunFile("main.go", source, nil); err != nil {
		t.Fatal(err)
	}
}

func TestFieldAddrCacheStableReceiver(t *testing.T) {
	const source = `package main

type record struct {
	value int
}

func update(r *record) {
	var first *int
	for i := 0; i < 8; i++ {
		addr := &r.value
		if first == nil {
			first = addr
		} else if addr != first {
			panic("stable receiver returned a different field address")
		}
		*addr += i + 1
	}
	if first != &r.value || r.value != 36 {
		panic("stable receiver updated the wrong field")
	}
}

func main() {
	r := &record{}
	update(r)
}
`
	runFieldAddrCacheProgram(t, source, 64)
}

func TestFieldAddrCacheReceiverChanges(t *testing.T) {
	const source = `package main

type record struct {
	value int
}

func updateAlternating(left, right *record) {
	var leftAddr, rightAddr *int
	for i := 0; i < 12; i++ {
		current := left
		if i&1 != 0 {
			current = right
		}
		addr := &current.value
		if current == left {
			if leftAddr == nil {
				leftAddr = addr
			} else if addr != leftAddr {
				panic("left field address changed")
			}
		} else {
			if rightAddr == nil {
				rightAddr = addr
			} else if addr != rightAddr {
				panic("right field address changed")
			}
		}
		*addr += i + 1
	}
	if leftAddr != &left.value || rightAddr != &right.value {
		panic("receiver change returned a stale field address")
	}
}

func main() {
	left := &record{}
	right := &record{}
	updateAlternating(left, right)
	if left.value != 36 || right.value != 42 {
		panic("receiver change updated the wrong object")
	}
}
`
	runFieldAddrCacheProgram(t, source, 64)
}

func TestFieldAddrCacheNilReceiver(t *testing.T) {
	const source = `package main

type record struct {
	value int
}

func nilAfterWarmReceiver(r *record) {
	for i := 0; i < 2; i++ {
		current := r
		if i != 0 {
			current = nil
		}
		addr := &current.value
		*addr++
	}
}

func main() {
	nilAfterWarmReceiver(&record{})
}
`
	ctx := ixgo.NewContext(0)
	_, err := ctx.RunFile("main.go", source, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid memory address or nil pointer dereference") {
		t.Fatalf("got error %v, want nil pointer dereference", err)
	}
}

func TestFieldAddrCacheFramePoolReuse(t *testing.T) {
	const source = `package main

type record struct {
	value int
}

func setField(r *record, value int) *int {
	addr := &r.value
	*addr = value
	return addr
}

func main() {
	items := [3]record{}
	for i := 0; i < 100; i++ {
		current := &items[i%len(items)]
		addr := setField(current, i)
		if addr != &current.value || current.value != i {
			panic("pooled frame returned a stale field address")
		}
	}
}
`
	runFieldAddrCacheProgram(t, source, 0)
}

func TestFieldAddrCacheRecursiveFrames(t *testing.T) {
	const source = `package main

type record struct {
	value int
	next  *record
}

func updateRecursive(r *record, depth int) {
	var own *int
	for pass := 0; pass < 2; pass++ {
		addr := &r.value
		if pass == 0 {
			own = addr
			if depth != 0 {
				updateRecursive(r.next, depth-1)
			}
		} else if addr != own {
			panic("recursive call contaminated its caller field address")
		}
		*addr += depth + 1
	}
}

func main() {
	tail := &record{}
	middle := &record{next: tail}
	head := &record{next: middle}
	updateRecursive(head, 2)
	if head.value != 6 || middle.value != 4 || tail.value != 2 {
		panic("recursive call updated the wrong frame receiver")
	}
}
`
	runFieldAddrCacheProgram(t, source, 0)
}
