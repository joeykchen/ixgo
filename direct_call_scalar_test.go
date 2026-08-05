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
	"go/token"
	"go/types"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/tools/go/ssa"
)

const testDirectCallScalarPkgPath = "ixgo.test/direct_call_scalar"

func testDirectCallScalarInt() int         { return 7 }
func testDirectCallScalarFloat64() float64 { return 1.5 }
func testDirectCallScalarBool() bool       { return true }
func testDirectCallScalarMixed() float64   { return 2.5 }

type directCallBoxTracker[T any] struct {
	calls  int
	boxed  int
	reuses int
	boxes  map[register]*reusableValue[T]
}

func (p *directCallBoxTracker[T]) set(ctx DirectCallContext, result T) {
	DirectCallSetResult(ctx, result)
	p.calls++
	boxed, ok := ctx.frame.stack[ctx.result].(*reusableValue[T])
	if !ok {
		return
	}
	p.boxed++
	if p.boxes == nil {
		p.boxes = make(map[register]*reusableValue[T])
	}
	if previous, ok := p.boxes[ctx.result]; ok {
		if previous == boxed {
			p.reuses++
		}
		return
	}
	p.boxes[ctx.result] = boxed
}

func registerDirectCallScalarPackage(
	ints *directCallBoxTracker[int],
	floats *directCallBoxTracker[float64],
	bools *directCallBoxTracker[bool],
	mixed *directCallBoxTracker[float64],
) {
	intKey := testDirectCallScalarPkgPath + ".Int"
	floatKey := testDirectCallScalarPkgPath + ".Float64"
	boolKey := testDirectCallScalarPkgPath + ".Bool"
	mixedKey := testDirectCallScalarPkgPath + ".Mixed"
	RegisterPackage(&Package{
		Name:       "scalar",
		Path:       testDirectCallScalarPkgPath,
		Interfaces: map[string]reflect.Type{},
		NamedTypes: map[string]reflect.Type{},
		AliasTypes: map[string]reflect.Type{},
		Vars:       map[string]reflect.Value{},
		Funcs: map[string]reflect.Value{
			"Int":     reflect.ValueOf(testDirectCallScalarInt),
			"Float64": reflect.ValueOf(testDirectCallScalarFloat64),
			"Bool":    reflect.ValueOf(testDirectCallScalarBool),
			"Mixed":   reflect.ValueOf(testDirectCallScalarMixed),
		},
		DirectCalls: map[string]DirectCallBinding{
			intKey: newDirectCallBinding(testDirectCallScalarInt, func(ctx DirectCallContext) {
				ints.set(ctx, testDirectCallScalarInt())
			}),
			floatKey: newDirectCallBinding(testDirectCallScalarFloat64, func(ctx DirectCallContext) {
				floats.set(ctx, testDirectCallScalarFloat64())
			}),
			boolKey: newDirectCallBinding(testDirectCallScalarBool, func(ctx DirectCallContext) {
				bools.set(ctx, testDirectCallScalarBool())
			}),
			mixedKey: newDirectCallBinding(testDirectCallScalarMixed, func(ctx DirectCallContext) {
				mixed.set(ctx, testDirectCallScalarMixed())
			}),
		},
		TypedConsts:   map[string]TypedConst{},
		UntypedConsts: map[string]UntypedConst{},
	})
	for _, key := range []string{intKey, floatKey, boolKey, mixedKey} {
		RegisterExternal(key, nil)
	}
}

func TestDirectCallScalarResultReuse(t *testing.T) {
	var ints directCallBoxTracker[int]
	var floats directCallBoxTracker[float64]
	var bools directCallBoxTracker[bool]
	var mixed directCallBoxTracker[float64]
	registerDirectCallScalarPackage(&ints, &floats, &bools, &mixed)

	const source = `package main

import scalar "ixgo.test/direct_call_scalar"

var sink float64

func main() {
	for range 16 {
		if scalar.Int()+1 != 8 {
			panic("int")
		}
		if scalar.Float64()*2 != 3 {
			panic("float64")
		}
		if !scalar.Bool() {
			panic("bool not")
		}
		if scalar.Bool() == false {
			panic("bool equal")
		}
		if scalar.Bool() {
		} else {
			panic("bool if")
		}
		value := scalar.Mixed()
		if value != 2.5 {
			panic("mixed")
		}
		sink = value
	}
}
`
	if _, err := NewContext(0).RunFile("main.go", source, nil); err != nil {
		t.Fatal(err)
	}

	for name, tracker := range map[string]struct {
		calls, boxed, reuses int
	}{
		"int":     {ints.calls, ints.boxed, ints.reuses},
		"float64": {floats.calls, floats.boxed, floats.reuses},
		"bool":    {bools.calls, bools.boxed, bools.reuses},
	} {
		if tracker.calls == 0 || tracker.boxed != tracker.calls {
			t.Errorf("%s result boxes = %d/%d; want every call boxed", name, tracker.boxed, tracker.calls)
		}
		if tracker.reuses == 0 {
			t.Errorf("%s result box was not reused", name)
		}
	}
	if mixed.calls == 0 || mixed.boxed != 0 {
		t.Fatalf("mixed-use result boxes = %d/%d; want ordinary results", mixed.boxed, mixed.calls)
	}
}

func TestDirectCallScalarAccessorsDoNotAllocate(t *testing.T) {
	fr := &frame{stack: make([]value, 3)}
	boolContext := DirectCallContext{frame: fr, result: 0, reuseResult: true}
	intContext := DirectCallContext{frame: fr, result: 1, reuseResult: true}
	floatContext := DirectCallContext{frame: fr, result: 2, reuseResult: true}
	DirectCallSetResult(boolContext, true)
	DirectCallSetResult(intContext, 42)
	DirectCallSetResult(floatContext, 1.5)

	if !fr.reusableBool(0) || fr.reusableInt(1) != 42 || fr.reusableFloat64(2) != 1.5 {
		t.Fatal("scalar accessors did not unwrap direct-call results")
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		DirectCallSetResult(boolContext, true)
		DirectCallSetResult(intContext, 42)
		DirectCallSetResult(floatContext, 1.5)
		_, _, _ = fr.reusableBool(0), fr.reusableInt(1), fr.reusableFloat64(2)
	}); allocs != 0 {
		t.Fatalf("steady-state scalar result allocs = %v; want 0", allocs)
	}
}

const testScalarPipelinePkgPath = "ixgo.test/scalar_pipeline"

func testScalarPipelineFloat64(float64) {}
func testScalarPipelineInt(int)         {}
func testScalarPipelineBool(bool)       {}
func testScalarPipelineMixed(float64)   {}

type scalarPipelineTracker[T any] struct {
	calls  int
	boxed  int
	reuses int
	last   T
	boxes  map[register]*reusableValue[T]
}

func (p *scalarPipelineTracker[T]) read(ctx DirectCallContext) {
	p.calls++
	p.last = DirectCallArg[T](ctx, 0)
	boxed, ok := ctx.frame.stack[ctx.args[0]].(*reusableValue[T])
	if !ok {
		return
	}
	p.boxed++
	if p.boxes == nil {
		p.boxes = make(map[register]*reusableValue[T])
	}
	if previous, ok := p.boxes[ctx.args[0]]; ok {
		if previous == boxed {
			p.reuses++
		}
		return
	}
	p.boxes[ctx.args[0]] = boxed
}

func registerScalarPipelinePackage(
	floats *scalarPipelineTracker[float64],
	ints *scalarPipelineTracker[int],
	bools *scalarPipelineTracker[bool],
	mixed *scalarPipelineTracker[float64],
) {
	floatKey := testScalarPipelinePkgPath + ".Float64"
	intKey := testScalarPipelinePkgPath + ".Int"
	boolKey := testScalarPipelinePkgPath + ".Bool"
	mixedKey := testScalarPipelinePkgPath + ".Mixed"
	RegisterPackage(&Package{
		Name:       "pipeline",
		Path:       testScalarPipelinePkgPath,
		Interfaces: map[string]reflect.Type{},
		NamedTypes: map[string]reflect.Type{},
		AliasTypes: map[string]reflect.Type{},
		Vars:       map[string]reflect.Value{},
		Funcs: map[string]reflect.Value{
			"Float64": reflect.ValueOf(testScalarPipelineFloat64),
			"Int":     reflect.ValueOf(testScalarPipelineInt),
			"Bool":    reflect.ValueOf(testScalarPipelineBool),
			"Mixed":   reflect.ValueOf(testScalarPipelineMixed),
		},
		DirectCalls: map[string]DirectCallBinding{
			floatKey: newDirectCallBinding(testScalarPipelineFloat64, floats.read),
			intKey:   newDirectCallBinding(testScalarPipelineInt, ints.read),
			boolKey:  newDirectCallBinding(testScalarPipelineBool, bools.read),
			mixedKey: newDirectCallBinding(testScalarPipelineMixed, mixed.read),
		},
		TypedConsts:   map[string]TypedConst{},
		UntypedConsts: map[string]UntypedConst{},
	})
	for _, key := range []string{floatKey, intKey, boolKey, mixedKey} {
		RegisterExternal(key, nil)
	}
}

func TestScalarProducerPipelineReuse(t *testing.T) {
	var floats scalarPipelineTracker[float64]
	var ints scalarPipelineTracker[int]
	var bools scalarPipelineTracker[bool]
	var mixed scalarPipelineTracker[float64]
	registerScalarPipelinePackage(&floats, &ints, &bools, &mixed)

	const source = `package main

import pipeline "ixgo.test/scalar_pipeline"

var globalFloat64 = 5.5
var globalInt = 7
var globalBool = true
var stored float64

func main() {
	for range 16 {
		pipeline.Float64((globalFloat64 - 1) * 2)
		pipeline.Int((globalInt + 1) * 2)
		pipeline.Bool(!(globalBool == false))
	}
	value := globalFloat64 - 2
	pipeline.Mixed(value)
	stored = value
}
`
	if _, err := NewContext(0).RunFile("main.go", source, nil); err != nil {
		t.Fatal(err)
	}

	for name, tracker := range map[string]struct {
		calls, boxed, reuses int
	}{
		"float64": {floats.calls, floats.boxed, floats.reuses},
		"int":     {ints.calls, ints.boxed, ints.reuses},
		"bool":    {bools.calls, bools.boxed, bools.reuses},
	} {
		if tracker.calls != 16 || tracker.boxed != tracker.calls || tracker.reuses == 0 {
			t.Errorf("%s pipeline calls/boxes/reuses = %d/%d/%d", name, tracker.calls, tracker.boxed, tracker.reuses)
		}
	}
	if floats.last != 9 || ints.last != 16 || !bools.last {
		t.Fatalf("pipeline values = %v, %v, %v", floats.last, ints.last, bools.last)
	}
	if mixed.calls != 1 || mixed.boxed != 0 || mixed.last != 3.5 {
		t.Fatalf("mixed pipeline calls/boxes/value = %d/%d/%v", mixed.calls, mixed.boxed, mixed.last)
	}
}

func TestScalarProducerPipelineRejectsNamedTypes(t *testing.T) {
	typ := types.NewNamed(types.NewTypeName(token.NoPos, nil, "namedFloat", nil), types.Typ[types.Float64], nil)
	if _, ok := reusableScalarKindOf(typ); ok {
		t.Fatal("named scalar type unexpectedly enabled reusable boxing")
	}
}

func TestScalarProducerPipelineBoxesEveryProducer(t *testing.T) {
	const source = `package main

var globalFloat64 = 5.5
var sink int

func main() {
	if (globalFloat64-1)*2 != 9 {
		sink = 1
	} else {
		sink = 2
	}
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

	boxed := 0
	for _, block := range pkg.Func("main").Blocks {
		for _, instr := range block.Instrs {
			var candidate ssa.Value
			switch instr := instr.(type) {
			case *ssa.UnOp:
				if instr.Op == token.MUL {
					if _, ok := reusableScalarKindOf(instr.Type()); ok {
						candidate = instr
					}
				}
			case *ssa.BinOp:
				kind, ok := reusableScalarKindOf(instr.X.Type())
				if ok && reusableScalarBinOpSupported(kind, instr.Op) {
					candidate = instr
				}
			}
			if candidate == nil {
				continue
			}
			if !canReuseValue(interp, candidate) {
				t.Errorf("producer %s is not boxed", candidate)
			}
			boxed++
		}
	}
	if boxed != 4 {
		t.Fatalf("boxed producer count = %d; want dereference, subtract, multiply, and compare", boxed)
	}
}

func TestScalarProducerPipelineFallbacks(t *testing.T) {
	var floats scalarPipelineTracker[float64]
	var ints scalarPipelineTracker[int]
	var bools scalarPipelineTracker[bool]
	var mixed scalarPipelineTracker[float64]
	registerScalarPipelinePackage(&floats, &ints, &bools, &mixed)

	const source = `package main

import pipeline "ixgo.test/scalar_pipeline"

var globalFloat64 = 5.5
var globalInt = 7
var stored float64

func returnFallback() float64 {
	value := globalFloat64 - 1
	pipeline.Float64(value)
	return value
}

func convertFallback() float64 {
	value := globalInt + 1
	pipeline.Int(value)
	return float64(value)
}

func storeFallback() {
	value := globalFloat64 - 2
	pipeline.Float64(value)
	stored = value
}

func phiFallback(flag bool) float64 {
	value := 0.0
	if flag {
		next := globalFloat64 + 1
		pipeline.Float64(next)
		value = next
	}
	pipeline.Float64(value)
	return value
}

func main() {
	if returnFallback() != 4.5 {
		panic("return")
	}
	if convertFallback() != 8 {
		panic("convert")
	}
	storeFallback()
	if stored != 3.5 {
		panic("store")
	}
	if phiFallback(true) != 6.5 {
		panic("phi")
	}
}
`
	if _, err := NewContext(0).RunFile("main.go", source, nil); err != nil {
		t.Fatal(err)
	}
	if floats.calls != 4 || floats.boxed != 0 {
		t.Fatalf("float fallback calls/boxes = %d/%d; want 4/0", floats.calls, floats.boxed)
	}
	if ints.calls != 1 || ints.boxed != 0 {
		t.Fatalf("int fallback calls/boxes = %d/%d; want 1/0", ints.calls, ints.boxed)
	}
}

func TestScalarProducerPipelineDebugRefFallback(t *testing.T) {
	var scalarInts directCallBoxTracker[int]
	var scalarFloats directCallBoxTracker[float64]
	var scalarBools directCallBoxTracker[bool]
	var scalarMixed directCallBoxTracker[float64]
	registerDirectCallScalarPackage(&scalarInts, &scalarFloats, &scalarBools, &scalarMixed)

	var floats scalarPipelineTracker[float64]
	var ints scalarPipelineTracker[int]
	var bools scalarPipelineTracker[bool]
	var mixed scalarPipelineTracker[float64]
	registerScalarPipelinePackage(&floats, &ints, &bools, &mixed)

	seen := map[string]int{}
	var retained []any
	ctx := NewContext(0)
	ctx.SetDebug(func(info *DebugInfo) {
		variable, value, ok := info.AsVar()
		if !ok {
			return
		}
		seen[variable.Name()]++
		if variable.Name() != "raw" && variable.Name() != "computed" {
			return
		}
		retained = append(retained, value)
		if _, ok := value.(float64); !ok {
			t.Errorf("debug variable %s has internal value %T; want float64", variable.Name(), value)
		}
	})
	// NewContext creates the SSA builder before SetDebug enables GlobalDebug.
	ctx.Builder.Reset()

	const source = `package main

import (
	scalar "ixgo.test/direct_call_scalar"
	pipeline "ixgo.test/scalar_pipeline"
)

func main() {
	for i := 0; i < 2; i++ {
		raw := scalar.Float64()
		pipeline.Float64(raw)
		computed := raw + float64(i)
		pipeline.Float64(computed)
	}
}
`
	if _, err := ctx.RunFile("main.go", source, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"raw", "computed"} {
		if seen[name] == 0 {
			t.Errorf("debug variable %s was not observed; saw %v", name, seen)
		}
	}
	for _, value := range retained {
		if _, ok := value.(float64); !ok {
			t.Fatalf("retained debug value changed to %T", value)
		}
	}
}

func TestReusableValuesDisabledForEvalCall(t *testing.T) {
	var scalarInts directCallBoxTracker[int]
	var scalarFloats directCallBoxTracker[float64]
	var scalarBools directCallBoxTracker[bool]
	var scalarMixed directCallBoxTracker[float64]
	registerDirectCallScalarPackage(&scalarInts, &scalarFloats, &scalarBools, &scalarMixed)

	var floats scalarPipelineTracker[float64]
	var ints scalarPipelineTracker[int]
	var bools scalarPipelineTracker[bool]
	var mixed scalarPipelineTracker[float64]
	registerScalarPipelinePackage(&floats, &ints, &bools, &mixed)

	ctx := NewContext(0)
	var results []any
	ctx.evalCallFn = func(_ *Interp, _ *ssa.Call, values ...any) {
		results = append(results, values...)
	}
	const source = `package main

import (
	scalar "ixgo.test/direct_call_scalar"
	pipeline "ixgo.test/scalar_pipeline"
)

func main() {
	pipeline.Float64(scalar.Float64() + 1)
}
`
	if _, err := ctx.RunFile("main.go", source, nil); err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("eval call hook did not observe any results")
	}
	for _, result := range results {
		if _, internal := result.(*reusableValue[float64]); internal {
			t.Fatalf("eval call hook observed internal value %T", result)
		}
	}
	if scalarFloats.boxed != 0 || floats.boxed != 0 {
		t.Fatalf("reusable values reached eval mode: producer=%d consumer=%d", scalarFloats.boxed, floats.boxed)
	}
}

func TestScalarProducerPipelineNilDeref(t *testing.T) {
	var floats scalarPipelineTracker[float64]
	var ints scalarPipelineTracker[int]
	var bools scalarPipelineTracker[bool]
	var mixed scalarPipelineTracker[float64]
	registerScalarPipelinePackage(&floats, &ints, &bools, &mixed)

	const source = `package main

import pipeline "ixgo.test/scalar_pipeline"

var pointer *float64

func main() {
	pipeline.Float64(*pointer + 1)
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

	boxedDeref := false
	for _, block := range pkg.Func("main").Blocks {
		for _, instr := range block.Instrs {
			unop, ok := instr.(*ssa.UnOp)
			if ok && unop.Op == token.MUL && unop.Type() == types.Typ[types.Float64] {
				boxedDeref = canReuseValue(interp, unop)
			}
		}
	}
	if !boxedDeref {
		t.Fatal("nil dereference did not select the scalar pipeline")
	}
	if _, err := ctx.RunInterp(interp, "main.go", nil); err == nil ||
		!strings.Contains(err.Error(), "invalid memory address or nil pointer dereference") {
		t.Fatalf("error = %v; want nil pointer dereference", err)
	}
	if floats.calls != 0 {
		t.Fatalf("direct consumer called %d times after nil dereference", floats.calls)
	}
}

func TestSetScalarResultDoesNotAllocate(t *testing.T) {
	fr := &frame{stack: make([]value, 3)}
	setReusableValue(fr, 0, true, true)
	setReusableValue(fr, 1, true, 42)
	setReusableValue(fr, 2, true, 1.5)
	boolBox := fr.stack[0].(*reusableValue[bool])
	intBox := fr.stack[1].(*reusableValue[int])
	floatBox := fr.stack[2].(*reusableValue[float64])

	if allocs := testing.AllocsPerRun(1000, func() {
		setReusableValue(fr, 0, true, false)
		setReusableValue(fr, 1, true, 43)
		setReusableValue(fr, 2, true, 2.5)
	}); allocs != 0 {
		t.Fatalf("steady-state scalar pipeline allocs = %v; want 0", allocs)
	}
	if fr.stack[0] != boolBox || fr.stack[1] != intBox || fr.stack[2] != floatBox {
		t.Fatal("scalar pipeline replaced a warmed result box")
	}
}
