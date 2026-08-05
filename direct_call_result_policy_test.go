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
	"testing"
)

func TestReusableValueNeedsBoxing(t *testing.T) {
	pkg := types.NewPackage("ixgo.test/direct_call_result_policy", "resultpolicy")
	named := func(name string, underlying types.Type) *types.Named {
		obj := types.NewTypeName(token.NoPos, pkg, name, nil)
		return types.NewNamed(obj, underlying, nil)
	}
	field := func(name string, typ types.Type) *types.Var {
		return types.NewField(token.NoPos, pkg, name, typ, false)
	}

	intType := types.Typ[types.Int]
	pointerType := types.NewPointer(intType)
	namedPointer := named("NamedPointer", pointerType)
	pointerStruct := types.NewStruct([]*types.Var{field("Value", namedPointer)}, nil)
	namedPointerStruct := named("NamedPointerStruct", pointerStruct)
	namedPointerArray := named("NamedPointerArray", types.NewArray(namedPointerStruct, 1))
	emptyInterface := types.NewInterfaceType(nil, nil)
	emptyInterface.Complete()
	functionType := types.NewSignatureType(nil, nil, nil, types.NewTuple(), types.NewTuple(), false)

	tests := []struct {
		name string
		typ  types.Type
		want bool
	}{
		{name: "bool", typ: types.Typ[types.Bool], want: true},
		{name: "int", typ: intType, want: true},
		{name: "named_int", typ: named("NamedInt", intType), want: true},
		{name: "float64", typ: types.Typ[types.Float64], want: true},
		{name: "string", typ: types.Typ[types.String], want: true},
		{name: "slice", typ: types.NewSlice(intType), want: true},
		{
			name: "multi_field_struct",
			typ:  types.NewStruct([]*types.Var{field("First", pointerType), field("Second", pointerType)}, nil),
			want: true,
		},
		{name: "single_non_direct_array", typ: types.NewArray(intType, 1), want: true},
		{name: "multi_element_array", typ: types.NewArray(pointerType, 2), want: true},
		{name: "pointer", typ: pointerType, want: false},
		{name: "named_pointer", typ: namedPointer, want: false},
		{name: "map", typ: types.NewMap(types.Typ[types.String], intType), want: false},
		{name: "channel", typ: types.NewChan(types.SendRecv, intType), want: false},
		{name: "function", typ: functionType, want: false},
		{name: "interface", typ: emptyInterface, want: false},
		{name: "named_interface", typ: named("NamedInterface", emptyInterface), want: false},
		{name: "unsafe_pointer", typ: types.Typ[types.UnsafePointer], want: false},
		{name: "single_direct_struct", typ: pointerStruct, want: false},
		{name: "named_single_direct_struct", typ: namedPointerStruct, want: false},
		{name: "single_direct_array", typ: types.NewArray(pointerType, 1), want: false},
		{
			name: "nested_direct_struct_array",
			typ:  types.NewStruct([]*types.Var{field("Value", types.NewArray(pointerStruct, 1))}, nil),
			want: false,
		},
		{name: "recursive_named_wrapper", typ: namedPointerArray, want: false},
		{
			name: "single_interface_struct",
			typ:  types.NewStruct([]*types.Var{field("Value", emptyInterface)}, nil),
			want: true,
		},
		{name: "single_interface_array", typ: types.NewArray(emptyInterface, 1), want: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := reusableValueNeedsBoxing(test.typ); got != test.want {
				t.Fatalf("reusableValueNeedsBoxing(%s) = %v; want %v", test.typ, got, test.want)
			}
		})
	}
}

type directCallResultPolicyStruct struct {
	First  int
	Second string
}

func testDirectCallResultRepresentation[T any](
	t *testing.T,
	typ types.Type,
	first, second T,
	valid func(T) bool,
) {
	t.Helper()
	fr := &frame{stack: make([]value, 1)}
	interp := &Interp{}
	reuseResult := reusableValueNeedsBoxing(typ)
	produce := func(value T) DirectCallAdapter {
		return func(ctx DirectCallContext) {
			DirectCallSetResult(ctx, value)
		}
	}

	interp.invokeDirectCallWithResultMode(fr, produce(first), 0, nil, reuseResult)
	reusable, isReusable := fr.stack[0].(*reusableValue[T])
	if isReusable != reuseResult {
		t.Fatalf("stored result type = %T; reuseResult = %v", fr.stack[0], reuseResult)
	}
	interp.invokeDirectCallWithResultMode(fr, produce(second), 0, nil, reuseResult)
	if isReusable && fr.stack[0] != reusable {
		t.Fatal("eligible result did not reuse its wrapper")
	}
	var got T
	consume := func(ctx DirectCallContext) {
		got = DirectCallArg[T](ctx, 0)
	}
	interp.invokeDirectCall(fr, consume, 0, []register{0})
	if !valid(got) {
		t.Fatalf("consumer received %#v from stored %T", got, fr.stack[0])
	}
}

func TestDirectCallResultRepresentationPolicy(t *testing.T) {
	intType := types.Typ[types.Int]
	stringType := types.Typ[types.String]
	field := func(name string, typ types.Type) *types.Var {
		return types.NewField(token.NoPos, nil, name, typ, false)
	}

	t.Run("pointer_stays_plain", func(t *testing.T) {
		first, second := 1, 2
		testDirectCallResultRepresentation(
			t,
			types.NewPointer(intType),
			&first,
			&second,
			func(got *int) bool { return got == &second },
		)
	})
	t.Run("interface_stays_plain", func(t *testing.T) {
		first, second := 1, 2
		typ := types.NewInterfaceType(nil, nil)
		typ.Complete()
		testDirectCallResultRepresentation(
			t,
			typ,
			any(&first),
			any(&second),
			func(got any) bool { return got == &second },
		)
	})
	t.Run("map_stays_plain", func(t *testing.T) {
		first := map[string]int{"value": 1}
		second := map[string]int{"value": 2}
		testDirectCallResultRepresentation(
			t,
			types.NewMap(stringType, intType),
			first,
			second,
			func(got map[string]int) bool { return got["value"] == 2 },
		)
	})
	t.Run("channel_stays_plain", func(t *testing.T) {
		first := make(chan int)
		second := make(chan int)
		testDirectCallResultRepresentation(
			t,
			types.NewChan(types.SendRecv, intType),
			first,
			second,
			func(got chan int) bool { return got == second },
		)
	})
	t.Run("function_stays_plain", func(t *testing.T) {
		first := func() int { return 1 }
		second := func() int { return 2 }
		result := types.NewTuple(types.NewVar(token.NoPos, nil, "", intType))
		testDirectCallResultRepresentation(
			t,
			types.NewSignatureType(nil, nil, nil, types.NewTuple(), result, false),
			first,
			second,
			func(got func() int) bool { return got() == 2 },
		)
	})
	t.Run("struct_reuses_wrapper", func(t *testing.T) {
		typ := types.NewStruct([]*types.Var{
			field("First", intType),
			field("Second", stringType),
		}, nil)
		testDirectCallResultRepresentation(
			t,
			typ,
			directCallResultPolicyStruct{First: 1, Second: "first"},
			directCallResultPolicyStruct{First: 2, Second: "second"},
			func(got directCallResultPolicyStruct) bool {
				return got.First == 2 && got.Second == "second"
			},
		)
	})
	t.Run("slice_reuses_wrapper", func(t *testing.T) {
		testDirectCallResultRepresentation(
			t,
			types.NewSlice(intType),
			[]int{1},
			[]int{2},
			func(got []int) bool { return len(got) == 1 && got[0] == 2 },
		)
	})
}

var (
	directCallResultPolicyStructSink  directCallResultPolicyStruct
	directCallResultPolicyPointerSink *int
)

func BenchmarkDirectCallResultPolicy(b *testing.B) {
	structType := types.NewStruct([]*types.Var{
		types.NewField(token.NoPos, nil, "First", types.Typ[types.Int], false),
		types.NewField(token.NoPos, nil, "Second", types.Typ[types.String], false),
	}, nil)
	pointerType := types.NewPointer(types.Typ[types.Int])
	structValue := directCallResultPolicyStruct{First: 42, Second: "result"}
	pointerValue := &structValue.First

	b.Run("eligible_struct", func(b *testing.B) {
		b.Run("policy", func(b *testing.B) {
			fr := &frame{stack: make([]value, 1)}
			ctx := DirectCallContext{frame: fr, result: 0, args: []register{0}, reuseResult: reusableValueNeedsBoxing(structType)}
			DirectCallSetResult(ctx, structValue)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				DirectCallSetResult(ctx, structValue)
				directCallResultPolicyStructSink = DirectCallArg[directCallResultPolicyStruct](ctx, 0)
			}
		})
		b.Run("plain", func(b *testing.B) {
			fr := &frame{stack: make([]value, 1)}
			ctx := DirectCallContext{frame: fr, result: 0, args: []register{0}}
			b.ReportAllocs()
			for b.Loop() {
				DirectCallSetResult(ctx, structValue)
				directCallResultPolicyStructSink = DirectCallArg[directCallResultPolicyStruct](ctx, 0)
			}
		})
	})
	b.Run("ineligible_pointer", func(b *testing.B) {
		b.Run("policy", func(b *testing.B) {
			fr := &frame{stack: make([]value, 1)}
			ctx := DirectCallContext{frame: fr, result: 0, args: []register{0}, reuseResult: reusableValueNeedsBoxing(pointerType)}
			b.ReportAllocs()
			for b.Loop() {
				DirectCallSetResult(ctx, pointerValue)
				directCallResultPolicyPointerSink = DirectCallArg[*int](ctx, 0)
			}
		})
		b.Run("forced_wrapper", func(b *testing.B) {
			fr := &frame{stack: make([]value, 1)}
			ctx := DirectCallContext{frame: fr, result: 0, args: []register{0}, reuseResult: true}
			DirectCallSetResult(ctx, pointerValue)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				DirectCallSetResult(ctx, pointerValue)
				directCallResultPolicyPointerSink = DirectCallArg[*int](ctx, 0)
			}
		})
	})
}
