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

package ixgo

import (
	"context"
	"errors"
	"fmt"
	"go/token"
	"go/types"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	directcalltest "github.com/goplus/ixgo/testdata/directcall"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

const (
	testDirectCallPkgPath     = "ixgo.test/direct_call"
	testDirectCallKey         = testDirectCallPkgPath + ".Add"
	testDirectCallHostPkgPath = "github.com/goplus/ixgo/testdata/directcall"
)

func directCallAdd(a, b int) int {
	return a + b
}

func directCallWrongAdd(a, b int) int {
	return a + b + 1
}

func directCallWrongCounterValue(*directcalltest.Counter) int {
	return -1
}

func newDirectCallBinding(target any, adapter DirectCallAdapter) DirectCallBinding {
	return DirectCallBinding{Target: reflect.ValueOf(target), Adapter: adapter}
}

func registerStaticDirectCallPackage(binding DirectCallBinding) {
	RegisterPackage(&Package{
		Name:       "directcall",
		Path:       testDirectCallPkgPath,
		Interfaces: map[string]reflect.Type{},
		NamedTypes: map[string]reflect.Type{},
		AliasTypes: map[string]reflect.Type{},
		Vars:       map[string]reflect.Value{},
		Funcs: map[string]reflect.Value{
			"Add": reflect.ValueOf(directCallAdd),
		},
		DirectCalls: map[string]DirectCallBinding{
			testDirectCallKey: binding,
		},
		TypedConsts:   map[string]TypedConst{},
		UntypedConsts: map[string]UntypedConst{},
	})
}

func registerHostDirectCallPackage(key string, binding DirectCallBinding) {
	RegisterPackage(&Package{
		Name:       "directcall",
		Path:       testDirectCallHostPkgPath,
		Interfaces: map[string]reflect.Type{},
		NamedTypes: map[string]reflect.Type{
			"Counter":         reflect.TypeOf(directcalltest.Counter(0)),
			"FallbackCounter": reflect.TypeOf(directcalltest.FallbackCounter(0)),
			"Recorder":        reflect.TypeOf(directcalltest.Recorder{}),
			"ValueCounter":    reflect.TypeOf(directcalltest.ValueCounter(0)),
		},
		AliasTypes: map[string]reflect.Type{},
		Vars:       map[string]reflect.Value{},
		Funcs:      map[string]reflect.Value{},
		DirectCalls: map[string]DirectCallBinding{
			key: binding,
		},
		TypedConsts:   map[string]TypedConst{},
		UntypedConsts: map[string]UntypedConst{},
	})
}

func registerHostCallbackDirectCallPackage(bindings map[string]DirectCallBinding) {
	RegisterPackage(&Package{
		Name:       "directcall",
		Path:       testDirectCallHostPkgPath,
		Interfaces: map[string]reflect.Type{},
		NamedTypes: map[string]reflect.Type{
			"Callback": reflect.TypeOf(directcalltest.Callback(nil)),
		},
		AliasTypes: map[string]reflect.Type{},
		Vars:       map[string]reflect.Value{},
		Funcs: map[string]reflect.Value{
			"Check":       reflect.ValueOf(directcalltest.Check),
			"CheckError":  reflect.ValueOf(directcalltest.CheckError),
			"Repeat":      reflect.ValueOf(directcalltest.Repeat),
			"RepeatNamed": reflect.ValueOf(directcalltest.RepeatNamed),
			"Retain":      reflect.ValueOf(directcalltest.Retain),
		},
		DirectCalls:   bindings,
		TypedConsts:   map[string]TypedConst{},
		UntypedConsts: map[string]UntypedConst{},
	})
}

type installedPackageLoader struct {
	Loader
	path string
	pkg  *Package
}

func (l *installedPackageLoader) Installed(path string) (*Package, bool) {
	if path == l.path {
		return l.pkg, true
	}
	return l.Loader.Installed(path)
}

func clearExternalCallOverride(t testing.TB, key string) {
	t.Helper()
	RegisterExternal(key, nil)
	t.Cleanup(func() { RegisterExternal(key, nil) })
}

func TestDirectCallContext(t *testing.T) {
	fr := &frame{stack: []value{nil, nil}}
	ctx := DirectCallContext{frame: fr, result: 1, args: []register{0}}
	if got := DirectCallArg[*int](ctx, 0); got != nil {
		t.Fatalf("nil argument = %v; want nil", got)
	}
	ctx.SetResult(42)
	if got := fr.reg(1); got != 42 {
		t.Fatalf("result = %v; want 42", got)
	}
}

func TestDirectCallTypedResultReuse(t *testing.T) {
	type result struct {
		value int
	}
	fr := &frame{stack: make([]value, 1)}
	ctx := DirectCallContext{frame: fr, result: 0, args: []register{0}, reuseResult: true}

	DirectCallSetResult(ctx, result{value: 1})
	boxed, ok := fr.stack[0].(*reusableValue[result])
	if !ok {
		t.Fatalf("boxed result type = %T", fr.stack[0])
	}
	if got := DirectCallArg[result](ctx, 0); got.value != 1 {
		t.Fatalf("boxed result = %v; want 1", got.value)
	}

	DirectCallSetResult(ctx, result{value: 2})
	if fr.stack[0] != boxed {
		t.Fatal("typed result box was not reused")
	}
	if got := DirectCallArg[result](ctx, 0); got.value != 2 {
		t.Fatalf("reused boxed result = %v; want 2", got.value)
	}

	ctx.reuseResult = false
	DirectCallSetResult(ctx, result{value: 3})
	if _, ok := fr.stack[0].(*reusableValue[result]); ok {
		t.Fatal("unboxed result retained the typed result box")
	}
}

func TestDirectCallFuncNativeFallback(t *testing.T) {
	type callback func()
	interp := &Interp{}
	var calls int
	native := callback(func() { calls++ })
	fr := &frame{interp: interp, stack: []value{native}}
	ctx := DirectCallContext{frame: fr, args: []register{0}}
	DirectCallFunc0[callback](ctx, 0)()
	if calls != 1 {
		t.Fatalf("native callback calls = %d; want 1", calls)
	}

	fr.stack[0] = func() bool { return true }
	if !DirectCallFunc0Result[func() bool](ctx, 0)() {
		t.Fatal("native predicate result = false; want true")
	}

	var nilCallback func()
	fr.stack[0] = nilCallback
	if got := DirectCallFunc0[func()](ctx, 0); got != nil {
		t.Fatal("nil native callback became non-nil")
	}
}

func TestDirectCallInterpretedCallbacks(t *testing.T) {
	keys := struct {
		check       string
		checkError  string
		repeat      string
		repeatNamed string
		retain      string
	}{
		check:       testDirectCallHostPkgPath + ".Check",
		checkError:  testDirectCallHostPkgPath + ".CheckError",
		repeat:      testDirectCallHostPkgPath + ".Repeat",
		repeatNamed: testDirectCallHostPkgPath + ".RepeatNamed",
		retain:      testDirectCallHostPkgPath + ".Retain",
	}
	for _, key := range []string{keys.check, keys.checkError, keys.repeat, keys.repeatNamed, keys.retain} {
		clearExternalCallOverride(t, key)
	}
	var interpretedCallbacks int
	var capturedCallback func()
	markInterpreted := func(ctx DirectCallContext, index int) {
		if directCallFuncValue(ctx, index) != nil {
			interpretedCallbacks++
		}
	}
	registerHostCallbackDirectCallPackage(map[string]DirectCallBinding{
		keys.check: newDirectCallBinding(directcalltest.Check, func(ctx DirectCallContext) {
			markInterpreted(ctx, 0)
			ctx.SetResult(directcalltest.Check(DirectCallFunc0Result[func() bool](ctx, 0)))
		}),
		keys.checkError: newDirectCallBinding(directcalltest.CheckError, func(ctx DirectCallContext) {
			markInterpreted(ctx, 0)
			ctx.SetResult(directcalltest.CheckError(DirectCallFunc0Result[func() error](ctx, 0)))
		}),
		keys.repeat: newDirectCallBinding(directcalltest.Repeat, func(ctx DirectCallContext) {
			markInterpreted(ctx, 1)
			directcalltest.Repeat(DirectCallArg[int](ctx, 0), DirectCallFunc0[func()](ctx, 1))
		}),
		keys.repeatNamed: newDirectCallBinding(directcalltest.RepeatNamed, func(ctx DirectCallContext) {
			markInterpreted(ctx, 1)
			directcalltest.RepeatNamed(DirectCallArg[int](ctx, 0), DirectCallFunc0[directcalltest.Callback](ctx, 1))
		}),
		keys.retain: newDirectCallBinding(directcalltest.Retain, func(ctx DirectCallContext) {
			markInterpreted(ctx, 0)
			capturedCallback = DirectCallArg[func()](ctx, 0)
			ctx.SetResult(directcalltest.Retain(DirectCallFunc0[func()](ctx, 0)))
		}),
	})

	const source = `package main

import host "github.com/goplus/ixgo/testdata/directcall"

func recoverCallbackPanic() (got any) {
	defer func() { got = recover() }()
	host.Repeat(1, func() { panic("boom") })
	return "not recovered"
}

func recoverDeferredCallback() (got any) {
	defer host.Retain(func() { got = recover() })()
	panic("deferred")
}

func main() {
	count := 0
	retained := host.Retain(func() { count++ })
	retained()
	host.Repeat(3, func() { count++ })
	host.RepeatNamed(2, host.Callback(func() { count++ }))
	if count != 6 {
		panic(count)
	}
	if !host.Check(func() bool { return count == 6 }) {
		panic("predicate failed")
	}
	if err := host.CheckError(func() error { return nil }); err != nil {
		panic(err)
	}
	if got := recoverCallbackPanic(); got != "boom" {
		panic(got)
	}
	if got := recoverDeferredCallback(); got != "deferred" {
		panic(got)
	}
}
`
	if _, err := NewContext(0).RunFile("main.go", source, nil); err != nil {
		t.Fatal(err)
	}
	if interpretedCallbacks != 7 {
		t.Fatalf("interpreted callback arguments = %d; want 7", interpretedCallbacks)
	}
	foreignInterp := &Interp{}
	foreignFrame := &frame{interp: foreignInterp, stack: []value{capturedCallback}}
	foreignContext := DirectCallContext{frame: foreignFrame, args: []register{0}}
	if call := directCallFuncValue(foreignContext, 0); call != nil {
		t.Fatal("callback from another interpreter used the direct path")
	}
	if callback := DirectCallFunc0[func()](foreignContext, 0); callback == nil {
		t.Fatal("callback from another interpreter was not preserved")
	}
}

func TestDirectCallCallbackAbort(t *testing.T) {
	repeatKey := testDirectCallHostPkgPath + ".Repeat"
	checkKey := testDirectCallHostPkgPath + ".Check"
	clearExternalCallOverride(t, repeatKey)
	clearExternalCallOverride(t, checkKey)

	callbackEntered := make(chan struct{})
	var signalOnce sync.Once
	var usedDirectCallback bool
	registerHostCallbackDirectCallPackage(map[string]DirectCallBinding{
		repeatKey: newDirectCallBinding(directcalltest.Repeat, func(ctx DirectCallContext) {
			usedDirectCallback = directCallFuncValue(ctx, 1) != nil
			directcalltest.Repeat(DirectCallArg[int](ctx, 0), DirectCallFunc0[func()](ctx, 1))
		}),
		checkKey: newDirectCallBinding(directcalltest.Check, func(ctx DirectCallContext) {
			signalOnce.Do(func() { close(callbackEntered) })
			ctx.SetResult(directcalltest.Check(DirectCallFunc0Result[func() bool](ctx, 0)))
		}),
	})

	const source = `package main

import host "github.com/goplus/ixgo/testdata/directcall"

func main() {
	host.Repeat(1, func() {
		host.Check(func() bool { return true })
		for {}
	})
}
`
	ctx := NewContext(0)
	pkg, err := ctx.LoadFile("main.go", source)
	if err != nil {
		t.Fatal(err)
	}
	interp, err := ctx.NewInterp(pkg)
	if err != nil {
		t.Fatal(err)
	}
	defer interp.UnsafeRelease()
	if err := interp.RunInit(); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := interp.RunMain()
		done <- err
	}()
	select {
	case <-callbackEntered:
	case <-time.After(time.Second):
		interp.Abort()
		t.Fatal("direct callback did not start")
	}
	if !usedDirectCallback {
		interp.Abort()
		t.Fatal("callback did not use the direct path")
	}

	interp.Abort()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunMain after Abort: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("direct callback ignored Abort")
	}
}

func TestDirectCallCallbackRunContextCancellation(t *testing.T) {
	repeatKey := testDirectCallHostPkgPath + ".Repeat"
	clearExternalCallOverride(t, repeatKey)

	callbackEntered := make(chan struct{})
	var signalOnce sync.Once
	var usedDirectCallback bool
	registerHostCallbackDirectCallPackage(map[string]DirectCallBinding{
		repeatKey: newDirectCallBinding(directcalltest.Repeat, func(ctx DirectCallContext) {
			usedDirectCallback = directCallFuncValue(ctx, 1) != nil
			signalOnce.Do(func() { close(callbackEntered) })
			directcalltest.Repeat(DirectCallArg[int](ctx, 0), DirectCallFunc0[func()](ctx, 1))
		}),
	})

	const source = `package main

import host "github.com/goplus/ixgo/testdata/directcall"

func main() {
	host.Repeat(1, func() { for {} })
}
`
	ctx := NewContext(0)
	pkg, err := ctx.LoadFile("main.go", source)
	if err != nil {
		t.Fatal(err)
	}
	interp, err := ctx.NewInterp(pkg)
	if err != nil {
		t.Fatal(err)
	}
	defer interp.UnsafeRelease()

	runContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx.RunContext = runContext
	go func() {
		select {
		case <-callbackEntered:
		case <-time.After(time.Second):
		}
		cancel()
	}()
	exitCode, err := ctx.RunInterp(interp, "main.go", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunInterp error = %v; want context.Canceled", err)
	}
	if exitCode != 0 {
		t.Fatalf("RunInterp exit code = %d; want 0", exitCode)
	}
	if !usedDirectCallback {
		t.Fatal("callback did not use the direct path")
	}
}

func TestDirectCallBindingRejectsNilTarget(t *testing.T) {
	var target func(int, int) int
	binding := newDirectCallBinding(target, func(DirectCallContext) {})
	if binding.valid() {
		t.Fatal("typed-nil function target was accepted")
	}
}

func TestDirectCallSignatureIncludesMethodReceiver(t *testing.T) {
	recv := types.NewVar(token.NoPos, nil, "recv", types.Typ[types.Int])
	param := types.NewVar(token.NoPos, nil, "v", types.Typ[types.String])
	result := types.NewVar(token.NoPos, nil, "", types.Typ[types.Bool])
	sig := types.NewSignature(recv, types.NewTuple(param), types.NewTuple(result), false)

	got := directCallSignature(&ssa.Function{Signature: sig})
	if got.Recv() != nil {
		t.Fatalf("direct-call signature still has receiver %v", got.Recv())
	}
	if got.Params().Len() != 2 || got.Params().At(0) != recv || got.Params().At(1) != param {
		t.Fatalf("direct-call params = %v; want (%v, %v)", got.Params(), recv, param)
	}
	if got.Results() != sig.Results() {
		t.Fatalf("direct-call results = %v; want %v", got.Results(), sig.Results())
	}
}

func TestDirectCallMethodKey(t *testing.T) {
	pkgPath, key, ok := directCallMethodKey(reflect.TypeOf((*directcalltest.Counter)(nil)), "Value")
	if !ok {
		t.Fatal("directCallMethodKey did not recognize a named pointer receiver")
	}
	if pkgPath != testDirectCallHostPkgPath {
		t.Fatalf("package path = %q; want %q", pkgPath, testDirectCallHostPkgPath)
	}
	want := "(*" + testDirectCallHostPkgPath + ".Counter).Value"
	if key != want {
		t.Fatalf("method key = %q; want %q", key, want)
	}
}

func TestInvokeSignatureMatchesExactTarget(t *testing.T) {
	receiver := reflect.TypeOf((*directcalltest.Counter)(nil))
	intType := reflect.TypeOf(int(0))
	signature := invokeSignature{
		params:  []reflect.Type{intType},
		results: []reflect.Type{intType},
	}
	tests := []struct {
		name   string
		target any
		match  bool
	}{
		{name: "exact", target: func(*directcalltest.Counter, int) int { return 0 }, match: true},
		{name: "receiver", target: func(directcalltest.Counter, int) int { return 0 }},
		{name: "parameter", target: func(*directcalltest.Counter, int64) int { return 0 }},
		{name: "result", target: func(*directcalltest.Counter, int) int64 { return 0 }},
		{name: "variadic", target: func(*directcalltest.Counter, ...int) int { return 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := signature.matches(reflect.TypeOf(test.target), receiver)
			if got != test.match {
				t.Fatalf("matches() = %v; want %v", got, test.match)
			}
		})
	}

	signature.params[0] = reflect.TypeOf([]int(nil))
	signature.variadic = true
	if !signature.matches(reflect.TypeOf(func(*directcalltest.Counter, ...int) int { return 0 }), receiver) {
		t.Fatal("variadic signature did not match an exact variadic target")
	}
}

func TestPackageDirectCallMerge(t *testing.T) {
	const (
		pkgPath = "ixgo.test/direct_call_merge"
		key     = pkgPath + ".Add"
	)
	RegisterPackage(&Package{
		Name:          "merge",
		Path:          pkgPath,
		Interfaces:    map[string]reflect.Type{},
		NamedTypes:    map[string]reflect.Type{},
		AliasTypes:    map[string]reflect.Type{},
		Vars:          map[string]reflect.Value{},
		Funcs:         map[string]reflect.Value{},
		TypedConsts:   map[string]TypedConst{},
		UntypedConsts: map[string]UntypedConst{},
	})
	want := newDirectCallBinding(directCallAdd, func(DirectCallContext) {})
	RegisterPackage(&Package{Path: pkgPath, DirectCalls: map[string]DirectCallBinding{key: want}})

	pkg, ok := LookupPackage(pkgPath)
	if !ok {
		t.Fatal("merged package was not registered")
	}
	if got, ok := pkg.DirectCalls[key]; !ok || got.Target != want.Target || got.Adapter == nil {
		t.Fatalf("merged direct func = (%v, %v); want metadata for %q", got, ok, key)
	}
}

func TestDirectCallBindingsAreSnapshotted(t *testing.T) {
	const (
		pkgPath = "ixgo.test/direct_call_snapshot"
		addKey  = pkgPath + ".Add"
		lateKey = pkgPath + ".Late"
	)
	initial := newDirectCallBinding(directCallAdd, func(DirectCallContext) {})
	RegisterPackage(&Package{
		Name:          "snapshot",
		Path:          pkgPath,
		Interfaces:    map[string]reflect.Type{},
		NamedTypes:    map[string]reflect.Type{},
		AliasTypes:    map[string]reflect.Type{},
		Vars:          map[string]reflect.Value{},
		Funcs:         map[string]reflect.Value{"Add": reflect.ValueOf(directCallAdd)},
		DirectCalls:   map[string]DirectCallBinding{addKey: initial},
		TypedConsts:   map[string]TypedConst{},
		UntypedConsts: map[string]UntypedConst{},
	})

	ctx := NewContext(DisableAutoLoadPatchs)
	if _, err := ctx.Loader.Import(pkgPath); err != nil {
		t.Fatal(err)
	}
	snapshot := snapshotDirectCallBindings(ctx.Loader, nil)
	RegisterPackage(&Package{Path: pkgPath, DirectCalls: map[string]DirectCallBinding{
		lateKey: newDirectCallBinding(directCallWrongAdd, func(DirectCallContext) {}),
	}})

	interp := &Interp{directCalls: snapshot}
	if got, ok := lookupDirectCallBinding(interp, pkgPath, addKey); !ok || got.Target != initial.Target {
		t.Fatalf("initial binding = (%v, %v); want snapshotted target", got, ok)
	}
	if _, ok := lookupDirectCallBinding(interp, pkgPath, lateKey); ok {
		t.Fatal("binding registered after the snapshot became visible")
	}
}

func TestStaticPackageCallUsesDirectCall(t *testing.T) {
	clearExternalCallOverride(t, testDirectCallKey)
	var calls int
	registerStaticDirectCallPackage(newDirectCallBinding(directCallAdd, func(ctx DirectCallContext) {
		calls++
		ctx.SetResult(directCallAdd(DirectCallArg[int](ctx, 0), DirectCallArg[int](ctx, 1)))
	}))

	const source = `package main

import directcall "ixgo.test/direct_call"

func main() {
	if got := directcall.Add(20, 22); got != 42 {
		panic(got)
	}
}
`
	if _, err := NewContext(0).RunFile("main.go", source, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("direct adapter calls = %d; want 1", calls)
	}
}

func TestPackageCallUsesInstalledPackage(t *testing.T) {
	clearExternalCallOverride(t, testDirectCallKey)
	registerStaticDirectCallPackage(newDirectCallBinding(directCallAdd, func(ctx DirectCallContext) {
		ctx.SetResult(-1)
	}))

	ctx := NewContext(DisableAutoLoadPatchs)
	if _, err := ctx.Loader.Import(testDirectCallPkgPath); err != nil {
		t.Fatal(err)
	}
	var directCalls int
	customAdd := func(a, b int) int { return a + b + 100 }
	customPackage := &Package{
		Name:  "directcall",
		Path:  testDirectCallPkgPath,
		Funcs: map[string]reflect.Value{"Add": reflect.ValueOf(customAdd)},
		DirectCalls: map[string]DirectCallBinding{
			testDirectCallKey: newDirectCallBinding(customAdd, func(ctx DirectCallContext) {
				directCalls++
				ctx.SetResult(customAdd(DirectCallArg[int](ctx, 0), DirectCallArg[int](ctx, 1)))
			}),
		},
	}
	ctx.Loader = &installedPackageLoader{Loader: ctx.Loader, path: testDirectCallPkgPath, pkg: customPackage}

	const source = `package main

import directcall "ixgo.test/direct_call"

func main() {
	if got := directcall.Add(20, 22); got != 142 {
		panic(got)
	}
}
`
	if _, err := ctx.RunFile("main.go", source, nil); err != nil {
		t.Fatal(err)
	}
	if directCalls != 1 {
		t.Fatalf("installed direct adapter calls = %d; want 1", directCalls)
	}
}

func TestGenericExternalCallKeepsInstantiatedKey(t *testing.T) {
	ctx := NewContext(0)
	ctx.RegisterExternal("main.Identity", func(int) int { return -1 })
	ctx.RegisterExternal("main.Identity[int]", func(value int) int { return value + 1 })

	const source = `package main

func Identity[T any](value T) T { return value }

func main() {
	_ = Identity[int](41)
}
`
	interp, err := ctx.LoadInterp("main.go", source)
	if err != nil {
		t.Fatal(err)
	}
	var instance *ssa.Function
	for fn := range ssautil.AllFunctions(interp.mainpkg.Prog) {
		if fn.String() == "main.Identity[int]" {
			instance = fn
			break
		}
	}
	if instance == nil {
		t.Fatal("generic function instance was not loaded")
	}
	external, ok := findExternFunc(interp, instance)
	if !ok {
		t.Fatal("instantiated external function was not resolved")
	}
	if got := external.Call([]reflect.Value{reflect.ValueOf(41)})[0].Interface(); got != 42 {
		t.Fatalf("external result = %v; want 42", got)
	}
}

func TestStaticMethodCallUsesDirectCall(t *testing.T) {
	key := "(*" + testDirectCallHostPkgPath + ".Counter).Value"
	clearExternalCallOverride(t, key)
	var calls int
	registerHostDirectCallPackage(key, newDirectCallBinding((*directcalltest.Counter).Value, func(ctx DirectCallContext) {
		calls++
		ctx.SetResult((*directcalltest.Counter).Value(DirectCallArg[*directcalltest.Counter](ctx, 0)))
	}))

	const source = `package main

import host "github.com/goplus/ixgo/testdata/directcall"

func main() {
	var counter host.Counter
	if counter.Value() != 0 {
		panic("unexpected counter value")
	}
}
`
	if _, err := NewContext(0).RunFile("main.go", source, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("direct adapter calls = %d; want 1", calls)
	}
}

func TestInvokeUsesDirectCall(t *testing.T) {
	key := "(*" + testDirectCallHostPkgPath + ".Counter).Value"
	clearExternalCallOverride(t, key)
	var calls int
	registerHostDirectCallPackage(key, newDirectCallBinding((*directcalltest.Counter).Value, func(ctx DirectCallContext) {
		calls++
		ctx.SetResult((*directcalltest.Counter).Value(DirectCallArg[*directcalltest.Counter](ctx, 0)))
	}))

	const source = `package main

import host "github.com/goplus/ixgo/testdata/directcall"

type counter interface {
	Value() int
}

func value(v counter) int {
	return v.Value()
}

func main() {
	var c host.Counter
	if value(&c) != 0 {
		panic("unexpected counter value")
	}
}
`
	if _, err := NewContext(0).RunFile("main.go", source, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("direct adapter calls = %d; want 1", calls)
	}
}

func TestInvokeCachesDirectAndFallbackTargets(t *testing.T) {
	key := "(*" + testDirectCallHostPkgPath + ".Counter).Value"
	clearExternalCallOverride(t, key)
	registerHostDirectCallPackage(key, newDirectCallBinding((*directcalltest.Counter).Value, func(ctx DirectCallContext) {
		ctx.SetResult((*directcalltest.Counter).Value(DirectCallArg[*directcalltest.Counter](ctx, 0)))
	}))

	intType := reflect.TypeOf(int(0))
	ctx := NewContext(DisableAutoLoadPatchs)
	if _, err := ctx.Loader.Import(testDirectCallHostPkgPath); err != nil {
		t.Fatal(err)
	}
	resolver := &invokeResolver{
		interp: &Interp{ctx: ctx, directCalls: snapshotDirectCallBindings(ctx.Loader, nil)},
		name:   "Value", signature: invokeSignature{results: []reflect.Type{intType}},
	}
	directType := reflect.TypeOf((*directcalltest.Counter)(nil))
	fallbackType := reflect.TypeOf((*directcalltest.FallbackCounter)(nil))

	direct := resolver.resolve(directType)
	if direct.direct == nil || direct.interpreted != nil || !direct.reflectFunc.IsValid() {
		t.Fatalf("direct target = %#v; want adapter", direct)
	}
	fallback := resolver.resolve(fallbackType)
	if fallback.direct != nil || fallback.interpreted != nil || !fallback.reflectFunc.IsValid() {
		t.Fatalf("fallback target = %#v; want external method", fallback)
	}

	for _, receiver := range []reflect.Type{directType, fallbackType, directType, fallbackType} {
		want, ok := resolver.targets.Load(receiver)
		if !ok {
			t.Fatalf("receiver %v was not cached", receiver)
		}
		got := resolver.resolve(receiver)
		cached := want.(*invokeCacheEntry).target
		if got.interpreted != cached.interpreted || (got.direct == nil) != (cached.direct == nil) || got.reflectFunc != cached.reflectFunc {
			t.Fatalf("receiver %v resolved a different target", receiver)
		}
	}
}

func TestInvokeResolverDoesNotCacheReflectxMethod(t *testing.T) {
	receiver := reflect.TypeOf((*directcalltest.FallbackCounter)(nil))
	resolver := &invokeResolver{
		interp: &Interp{msets: map[reflect.Type]map[string]*ssa.Function{receiver: {}}},
		name:   "Value",
	}

	if target := resolver.resolve(receiver); !target.reflectFunc.IsValid() {
		t.Fatal("reflectx method was not resolved")
	}
	if _, ok := resolver.targets.Load(receiver); ok || resolver.recent.Load() != nil {
		t.Fatal("reflectx-owned method was cached across ResetIcall")
	}
}

func TestInvokeUsesDirectAndFallbackTargets(t *testing.T) {
	key := "(*" + testDirectCallHostPkgPath + ".Counter).Value"
	clearExternalCallOverride(t, key)
	var directCalls int
	registerHostDirectCallPackage(key, newDirectCallBinding((*directcalltest.Counter).Value, func(ctx DirectCallContext) {
		directCalls++
		ctx.SetResult((*directcalltest.Counter).Value(DirectCallArg[*directcalltest.Counter](ctx, 0)))
	}))

	const source = `package main

import host "github.com/goplus/ixgo/testdata/directcall"

type counter interface {
	Value() int
}

func value(v counter) int {
	return v.Value()
}

func main() {
	direct := host.Counter(7)
	fallback := host.FallbackCounter(9)
	for i := 0; i < 2; i++ {
		if value(&direct) != 7 || value(&fallback) != 9 {
			panic("unexpected counter value")
		}
	}
}
`
	if _, err := NewContext(0).RunFile("main.go", source, nil); err != nil {
		t.Fatal(err)
	}
	if directCalls != 2 {
		t.Fatalf("direct adapter calls = %d; want 2", directCalls)
	}
}

func TestInvokeResolverConcurrentColdCache(t *testing.T) {
	key := "(*" + testDirectCallHostPkgPath + ".Counter).Value"
	clearExternalCallOverride(t, key)
	registerHostDirectCallPackage(key, newDirectCallBinding((*directcalltest.Counter).Value, func(ctx DirectCallContext) {
		ctx.SetResult((*directcalltest.Counter).Value(DirectCallArg[*directcalltest.Counter](ctx, 0)))
	}))

	ctx := NewContext(DisableAutoLoadPatchs)
	if _, err := ctx.Loader.Import(testDirectCallHostPkgPath); err != nil {
		t.Fatal(err)
	}
	interp := &Interp{ctx: ctx, directCalls: snapshotDirectCallBindings(ctx.Loader, nil)}
	intType := reflect.TypeOf(int(0))
	tests := []struct {
		name        string
		receiver    reflect.Type
		wantDirect  bool
		wantReflect bool
	}{
		{name: "direct", receiver: reflect.TypeOf((*directcalltest.Counter)(nil)), wantDirect: true, wantReflect: true},
		{name: "reflect", receiver: reflect.TypeOf((*directcalltest.FallbackCounter)(nil)), wantReflect: true},
		{name: "miss", receiver: intType},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := &invokeResolver{
				interp: interp,
				name:   "Value",
				signature: invokeSignature{
					results: []reflect.Type{intType},
				},
			}
			const goroutines = 32
			start := make(chan struct{})
			results := make(chan invokeTarget, goroutines)
			var workers sync.WaitGroup
			workers.Add(goroutines)
			for range goroutines {
				go func() {
					defer workers.Done()
					<-start
					results <- resolver.resolve(test.receiver)
				}()
			}
			close(start)
			workers.Wait()
			close(results)
			for target := range results {
				if got := target.direct != nil; got != test.wantDirect {
					t.Fatalf("direct target = %v; want %v", got, test.wantDirect)
				}
				if got := target.reflectFunc.IsValid(); got != test.wantReflect {
					t.Fatalf("reflect target = %v; want %v", got, test.wantReflect)
				}
			}
		})
	}
}

func TestDynamicValueReceiverPointerUsesDirectCall(t *testing.T) {
	key := "(*" + testDirectCallHostPkgPath + ".ValueCounter).Value"
	clearExternalCallOverride(t, key)
	var calls int
	registerHostDirectCallPackage(key, newDirectCallBinding((*directcalltest.ValueCounter).Value, func(ctx DirectCallContext) {
		calls++
		ctx.SetResult((*directcalltest.ValueCounter).Value(DirectCallArg[*directcalltest.ValueCounter](ctx, 0)))
	}))

	const source = `package main

import host "github.com/goplus/ixgo/testdata/directcall"

type counter interface {
	Value() int
}

func main() {
	value := host.ValueCounter(7)
	var counter counter = &value
	if got := counter.Value(); got != 7 {
		panic(got)
	}
}
`
	if _, err := NewContext(0).RunFile("main.go", source, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("direct adapter calls = %d; want 1", calls)
	}
}

func TestInvokeRequiresResolvedTarget(t *testing.T) {
	key := "(*" + testDirectCallHostPkgPath + ".Counter).Value"
	clearExternalCallOverride(t, key)
	var directCalls int
	registerHostDirectCallPackage(key, newDirectCallBinding(directCallWrongCounterValue, func(ctx DirectCallContext) {
		directCalls++
		ctx.SetResult(-1)
	}))

	const source = `package main

import host "github.com/goplus/ixgo/testdata/directcall"

type counter interface {
	Value() int
}

func value(v counter) int {
	return v.Value()
}

func main() {
	counter := host.Counter(7)
	if got := value(&counter); got != 7 {
		panic(got)
	}
}
`
	if _, err := NewContext(0).RunFile("main.go", source, nil); err != nil {
		t.Fatal(err)
	}
	if directCalls != 0 {
		t.Fatalf("mismatched direct adapter calls = %d; want 0", directCalls)
	}
}

func TestInvokeGoAndDeferPreserveCapturedArguments(t *testing.T) {
	key := "(*" + testDirectCallHostPkgPath + ".Recorder).Record"
	clearExternalCallOverride(t, key)
	var directCalls int
	registerHostDirectCallPackage(key, newDirectCallBinding((*directcalltest.Recorder).Record, func(ctx DirectCallContext) {
		directCalls++
		(*directcalltest.Recorder).Record(
			DirectCallArg[*directcalltest.Recorder](ctx, 0),
			DirectCallArg[int](ctx, 1),
		)
	}))

	const source = `package main

import host "github.com/goplus/ixgo/testdata/directcall"

type recorder interface {
	Record(int)
}

func values(yield func(int) bool) {
	for value := 0; value < 3; value++ {
		if !yield(value) {
			return
		}
	}
}

func main() {
	first := host.Recorder{Values: make(chan int, 4)}
	second := host.Recorder{Values: make(chan int, 4)}
	var sink recorder = &first

	value := 7
	go sink.Record(value)
	sink = &second
	value = 8
	if got := <-first.Values; got != 7 {
		panic(got)
	}
	select {
	case got := <-second.Values:
		panic(got)
	default:
	}

	sink = &first
	defer func() {
		for _, want := range []int{2, 1, 0} {
			if got := <-first.Values; got != want {
				panic(got)
			}
		}
		select {
		case got := <-second.Values:
			panic(got)
		default:
		}
	}()
	for value := range values {
		defer sink.Record(value)
	}
	sink = &second
}
`
	ctx := NewContext(0)
	interp, err := ctx.LoadInterp("main.go", source)
	if err != nil {
		t.Fatal(err)
	}
	var hasDeferStack bool
	for fn := range ssautil.AllFunctions(interp.mainpkg.Prog) {
		for _, block := range fn.Blocks {
			for _, instruction := range block.Instrs {
				if instruction, ok := instruction.(*ssa.Defer); ok && instruction.DeferStack != nil {
					hasDeferStack = true
				}
			}
		}
	}
	if !hasDeferStack {
		t.Fatal("test program did not exercise the DeferStack path")
	}
	if _, err := interp.RunMain(); err != nil {
		t.Fatal(err)
	}
	if directCalls != 0 {
		t.Fatalf("delayed direct adapter calls = %d; want reflective fallback", directCalls)
	}
}

func TestInvokeGoAndDeferInterpretedMethod(t *testing.T) {
	const source = `package main

type recorder interface {
	Record(int)
}

type localRecorder struct {
	Values chan int
}

func (r *localRecorder) Record(value int) {
	r.Values <- value
}

func main() {
	first := localRecorder{Values: make(chan int, 3)}
	second := localRecorder{Values: make(chan int, 3)}
	var sink recorder = &first

	value := 7
	go sink.Record(value)
	sink = &second
	value = 8
	if got := <-first.Values; got != 7 {
		panic(got)
	}

	sink = &first
	defer func() {
		if got := <-first.Values; got != 2 {
			panic(got)
		}
		select {
		case got := <-second.Values:
			panic(got)
		default:
		}
	}()
	defer sink.Record(2)
	sink = &second
}
`
	if _, err := NewContext(0).RunFile("main.go", source, nil); err != nil {
		t.Fatal(err)
	}
}

func TestInvokeGoAndDeferNilReceiver(t *testing.T) {
	for _, statement := range []string{"go sink.Record(1)", "defer sink.Record(1)"} {
		t.Run(strings.Fields(statement)[0], func(t *testing.T) {
			source := fmt.Sprintf(`package main

type recorder interface {
	Record(int)
}

func main() {
	var sink recorder
	%s
}
`, statement)
			if _, err := NewContext(0).RunFile("main.go", source, nil); err == nil ||
				!strings.Contains(err.Error(), "invalid memory address or nil pointer dereference") {
				t.Fatalf("error = %v; want nil-interface runtime error", err)
			}
		})
	}
}

func TestContextOverrideDoesNotAffectInterfaceInvoke(t *testing.T) {
	key := "(*" + testDirectCallHostPkgPath + ".Counter).Value"
	clearExternalCallOverride(t, key)
	var directCalls, overrideCalls int
	registerHostDirectCallPackage(key, newDirectCallBinding((*directcalltest.Counter).Value, func(ctx DirectCallContext) {
		directCalls++
		ctx.SetResult((*directcalltest.Counter).Value(DirectCallArg[*directcalltest.Counter](ctx, 0)))
	}))
	ctx := NewContext(0)
	ctx.RegisterExternal(key, func(*directcalltest.Counter) int {
		overrideCalls++
		return -1
	})

	const source = `package main

import host "github.com/goplus/ixgo/testdata/directcall"

type counter interface {
	Value() int
}

func value(v counter) int {
	return v.Value()
}

func main() {
	counter := host.Counter(7)
	if got := value(&counter); got != 7 {
		panic(got)
	}
}
`
	if _, err := ctx.RunFile("main.go", source, nil); err != nil {
		t.Fatal(err)
	}
	if directCalls != 1 || overrideCalls != 0 {
		t.Fatalf("calls: direct=%d override=%d; want 1, 0", directCalls, overrideCalls)
	}
}

func TestRegisteredExternalDoesNotAffectInterfaceInvoke(t *testing.T) {
	key := "(*" + testDirectCallHostPkgPath + ".Counter).Value"
	clearExternalCallOverride(t, key)
	var directCalls, overrideCalls int
	registerHostDirectCallPackage(key, newDirectCallBinding((*directcalltest.Counter).Value, func(ctx DirectCallContext) {
		directCalls++
		ctx.SetResult((*directcalltest.Counter).Value(DirectCallArg[*directcalltest.Counter](ctx, 0)))
	}))
	RegisterExternal(key, func(*directcalltest.Counter) int {
		overrideCalls++
		return -1
	})

	const source = `package main

import host "github.com/goplus/ixgo/testdata/directcall"

type counter interface {
	Value() int
}

func main() {
	hostCounter := host.Counter(7)
	var value counter = &hostCounter
	if got := value.Value(); got != 7 {
		panic(got)
	}
}
`
	if _, err := NewContext(0).RunFile("main.go", source, nil); err != nil {
		t.Fatal(err)
	}
	if directCalls != 1 || overrideCalls != 0 {
		t.Fatalf("calls: direct=%d override=%d; want 1, 0", directCalls, overrideCalls)
	}
}

func TestContextOverrideSkipsPackageDirectCall(t *testing.T) {
	clearExternalCallOverride(t, testDirectCallKey)
	var directCalls, overrideCalls int
	registerStaticDirectCallPackage(newDirectCallBinding(directCallAdd, func(ctx DirectCallContext) {
		directCalls++
		ctx.SetResult(-1)
	}))
	ctx := NewContext(0)
	ctx.RegisterExternal(testDirectCallKey, func(a, b int) int {
		overrideCalls++
		return a + b + 1
	})

	const source = `package main

import directcall "ixgo.test/direct_call"

func main() {
	if got := directcall.Add(20, 22); got != 43 {
		panic(got)
	}
}
`
	if _, err := ctx.RunFile("main.go", source, nil); err != nil {
		t.Fatal(err)
	}
	if directCalls != 0 || overrideCalls != 1 {
		t.Fatalf("calls: direct=%d override=%d; want 0, 1", directCalls, overrideCalls)
	}
}

func TestRegisteredExternalSkipsPackageDirectCall(t *testing.T) {
	clearExternalCallOverride(t, testDirectCallKey)
	var directCalls, overrideCalls int
	registerStaticDirectCallPackage(newDirectCallBinding(directCallAdd, func(ctx DirectCallContext) {
		directCalls++
		ctx.SetResult(-1)
	}))
	RegisterExternal(testDirectCallKey, func(a, b int) int {
		overrideCalls++
		return a + b + 1
	})

	const source = `package main

import directcall "ixgo.test/direct_call"

func main() {
	if got := directcall.Add(20, 22); got != 43 {
		panic(got)
	}
}
`
	if _, err := NewContext(0).RunFile("main.go", source, nil); err != nil {
		t.Fatal(err)
	}
	if directCalls != 0 || overrideCalls != 1 {
		t.Fatalf("calls: direct=%d override=%d; want 0, 1", directCalls, overrideCalls)
	}
}

func TestPackageDirectCallRequiresResolvedTarget(t *testing.T) {
	clearExternalCallOverride(t, testDirectCallKey)
	var directCalls int
	registerStaticDirectCallPackage(newDirectCallBinding(directCallWrongAdd, func(ctx DirectCallContext) {
		directCalls++
		ctx.SetResult(-1)
	}))

	const source = `package main

import directcall "ixgo.test/direct_call"

func main() {
	if got := directcall.Add(20, 22); got != 42 {
		panic(got)
	}
}
`
	if _, err := NewContext(0).RunFile("main.go", source, nil); err != nil {
		t.Fatal(err)
	}
	if directCalls != 0 {
		t.Fatalf("direct adapter calls = %d; want 0", directCalls)
	}
}

func TestPackageDirectCallRequiresExactSignature(t *testing.T) {
	clearExternalCallOverride(t, testDirectCallKey)
	var directCalls int
	wrong := func(a, b int64) int64 { return a + b }
	registerStaticDirectCallPackage(newDirectCallBinding(wrong, func(ctx DirectCallContext) {
		directCalls++
		ctx.SetResult(int64(-1))
	}))

	const source = `package main

import directcall "ixgo.test/direct_call"

func main() {
	if got := directcall.Add(20, 22); got != 42 {
		panic(got)
	}
}
`
	if _, err := NewContext(0).RunFile("main.go", source, nil); err != nil {
		t.Fatal(err)
	}
	if directCalls != 0 {
		t.Fatalf("direct adapter calls = %d; want 0", directCalls)
	}
}

func TestDirectCallPreservesDeferFrame(t *testing.T) {
	adapter := DirectCallAdapter(func(ctx DirectCallContext) {
		ctx.SetResult(directCallAdd(DirectCallArg[int](ctx, 0), DirectCallArg[int](ctx, 1)))
	})
	interp := &Interp{}
	fr := &frame{stack: []value{20, 22, nil}, deferid: 7}
	interp.invokeDirectCall(fr, adapter, 2, []register{0, 1})
	if got := fr.reg(2); got != 42 {
		t.Fatalf("result = %v; want 42", got)
	}
	got, ok := interp.deferMap.Load(uint64(7))
	if !ok || got != fr {
		t.Fatalf("defer frame = (%v, %v); want (%p, true)", got, ok, fr)
	}
}

func BenchmarkDirectCall(b *testing.B) {
	fnValue := reflect.ValueOf(directCallAdd)
	registerStaticDirectCallPackage(DirectCallBinding{
		Target: fnValue,
		Adapter: func(ctx DirectCallContext) {
			ctx.SetResult(directCallAdd(DirectCallArg[int](ctx, 0), DirectCallArg[int](ctx, 1)))
		},
	})
	RegisterExternal(testDirectCallKey, nil)
	interp := &Interp{preloadTypes: map[types.Type]reflect.Type{
		types.Typ[types.Int]: reflect.TypeOf(int(0)),
	}}
	fr := &frame{stack: []value{20, 22, nil}}
	ia := []register{0, 1}

	b.Run("direct", func(b *testing.B) {
		adapter := DirectCallAdapter(func(ctx DirectCallContext) {
			ctx.SetResult(directCallAdd(DirectCallArg[int](ctx, 0), DirectCallArg[int](ctx, 1)))
		})
		call := func(fr *frame) {
			interp.invokeDirectCall(fr, adapter, 2, ia)
		}
		b.ReportAllocs()
		for b.Loop() {
			call(fr)
		}
	})
	b.Run("reflect", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			interp.callExternalByStack(fr, fnValue, 2, ia)
		}
	})

	methodKey := "(*" + testDirectCallHostPkgPath + ".Counter).Value"
	registerHostDirectCallPackage(methodKey, newDirectCallBinding((*directcalltest.Counter).Value, func(ctx DirectCallContext) {
		ctx.SetResult((*directcalltest.Counter).Value(DirectCallArg[*directcalltest.Counter](ctx, 0)))
	}))
	ctx := NewContext(DisableAutoLoadPatchs)
	if _, err := ctx.Loader.Import(testDirectCallHostPkgPath); err != nil {
		b.Fatal(err)
	}
	interp.ctx = ctx
	interp.directCalls = snapshotDirectCallBindings(ctx.Loader, nil)
	result := types.NewVar(token.NoPos, nil, "", types.Typ[types.Int])
	method := types.NewFunc(token.NoPos, nil, "Value", types.NewSignature(nil, types.NewTuple(), types.NewTuple(result), false))
	methodCall := &ssa.CallCommon{Method: method}
	var counter directcalltest.Counter
	methodFrame := &frame{stack: []value{&counter, nil}}
	methodArgs := []register{0}

	b.Run("dynamic_direct", func(b *testing.B) {
		call := makeInvokeInstr(interp, nil, methodCall, 1, 0, nil)
		b.ReportAllocs()
		for b.Loop() {
			call(methodFrame)
		}
	})
	var fallback directcalltest.FallbackCounter
	fallbackFrame := &frame{stack: []value{&fallback, nil}}
	b.Run("dynamic_fallback", func(b *testing.B) {
		call := makeInvokeInstr(interp, nil, methodCall, 1, 0, nil)
		b.ReportAllocs()
		for b.Loop() {
			call(fallbackFrame)
		}
	})
	b.Run("dynamic_reflect", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			fn, ok := findExternMethod(reflect.TypeOf(methodFrame.reg(0)), "Value")
			if !ok {
				b.Fatal("method not found")
			}
			interp.callExternalByStack(methodFrame, fn, 1, methodArgs)
		}
	})
}

func BenchmarkDirectCallCallback(b *testing.B) {
	key := testDirectCallHostPkgPath + ".Retain"
	clearExternalCallOverride(b, key)
	var reflectedCallback func()
	var directCallback func()
	var callbackInterp *Interp
	registerHostCallbackDirectCallPackage(map[string]DirectCallBinding{
		key: newDirectCallBinding(directcalltest.Retain, func(ctx DirectCallContext) {
			callbackInterp = ctx.frame.interp
			reflectedCallback = DirectCallArg[func()](ctx, 0)
			directCallback = DirectCallFunc0[func()](ctx, 0)
			ctx.SetResult(directcalltest.Retain(directCallback))
		}),
	})

	const source = `package main

import host "github.com/goplus/ixgo/testdata/directcall"

var sink int
var retained func()

func main() {
	retained = host.Retain(func() { sink++ })
}
`
	if _, err := NewContext(0).RunFile("main.go", source, nil); err != nil {
		b.Fatal(err)
	}
	if callbackInterp == nil || reflectedCallback == nil || directCallback == nil {
		b.Fatal("callback adapter did not capture both paths")
	}
	callbackFrame := &frame{interp: callbackInterp, stack: []value{reflectedCallback}}
	callbackContext := DirectCallContext{frame: callbackFrame, args: []register{0}}
	if directCallFuncValue(callbackContext, 0) == nil {
		b.Fatal("captured callback is not recognized as interpreted")
	}

	b.Run("create", func(b *testing.B) {
		b.Run("reflect", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				callback := DirectCallArg[func()](callbackContext, 0)
				runtime.KeepAlive(callback)
			}
		})
		b.Run("direct", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				callback := DirectCallFunc0[func()](callbackContext, 0)
				runtime.KeepAlive(callback)
			}
		})
	})
	b.Run("once", func(b *testing.B) {
		b.Run("reflect", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				DirectCallArg[func()](callbackContext, 0)()
			}
		})
		b.Run("direct", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				DirectCallFunc0[func()](callbackContext, 0)()
			}
		})
	})
	b.Run("repeat", func(b *testing.B) {
		const callsPerIteration = 16
		b.Run("reflect", func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(callsPerIteration, "calls/op")
			for b.Loop() {
				callback := DirectCallArg[func()](callbackContext, 0)
				for range callsPerIteration {
					callback()
				}
			}
		})
		b.Run("direct", func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(callsPerIteration, "calls/op")
			for b.Loop() {
				callback := DirectCallFunc0[func()](callbackContext, 0)
				for range callsPerIteration {
					callback()
				}
			}
		})
	})
	b.Run("retain", func(b *testing.B) {
		b.Run("reflect", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				reflectedCallback()
			}
		})
		b.Run("direct", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				directCallback()
			}
		})
	})
}
