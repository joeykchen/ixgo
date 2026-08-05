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

	"golang.org/x/tools/go/ssa"
)

type reusableScalarKind uint8

const (
	reusableScalarInvalid reusableScalarKind = iota
	reusableScalarBool
	reusableScalarInt
	reusableScalarFloat64
)

func reusableScalarKindOf(typ types.Type) (reusableScalarKind, bool) {
	basic, ok := types.Unalias(typ).(*types.Basic)
	if !ok {
		return reusableScalarInvalid, false
	}
	switch basic.Kind() {
	case types.Bool:
		return reusableScalarBool, true
	case types.Int:
		return reusableScalarInt, true
	case types.Float64:
		return reusableScalarFloat64, true
	default:
		return reusableScalarInvalid, false
	}
}

func reusableScalarConsumerSupported(kind reusableScalarKind, referrer ssa.Instruction, result ssa.Value) bool {
	switch referrer := referrer.(type) {
	case *ssa.BinOp:
		return (referrer.X == result || referrer.Y == result) && reusableScalarBinOpSupported(kind, referrer.Op)
	case *ssa.If:
		return kind == reusableScalarBool && referrer.Cond == result
	case *ssa.UnOp:
		return kind == reusableScalarBool && referrer.Op == token.NOT && referrer.X == result
	default:
		return false
	}
}

func reusableScalarBinOpSupported(kind reusableScalarKind, op token.Token) bool {
	switch kind {
	case reusableScalarBool:
		return op == token.EQL || op == token.NEQ
	case reusableScalarInt:
		switch op {
		case token.ADD, token.SUB, token.MUL, token.QUO, token.REM,
			token.AND, token.OR, token.XOR, token.AND_NOT,
			token.LSS, token.LEQ, token.GTR, token.GEQ, token.EQL, token.NEQ:
			return true
		}
	case reusableScalarFloat64:
		switch op {
		case token.ADD, token.SUB, token.MUL, token.QUO,
			token.LSS, token.LEQ, token.GTR, token.GEQ, token.EQL, token.NEQ:
			return true
		}
	}
	return false
}

func isStaticDirectCallValue(interp *Interp, call *ssa.Call) bool {
	fn, ok := call.Call.Value.(*ssa.Function)
	if !ok || fn.Blocks != nil {
		return false
	}
	resolved, ok := findExternFunc(interp, fn)
	if !ok {
		return false
	}
	_, ok = resolveStaticDirectCall(interp, fn, resolved)
	return ok
}

func reusableValueProducerSupported(interp *Interp, value ssa.Value) bool {
	switch value := value.(type) {
	case *ssa.Call:
		return reusableValueNeedsBoxing(value.Type()) && isStaticDirectCallValue(interp, value)
	case *ssa.BinOp:
		kind, ok := reusableScalarKindOf(value.X.Type())
		return ok && reusableScalarBinOpSupported(kind, value.Op)
	case *ssa.UnOp:
		kind, ok := reusableScalarKindOf(value.Type())
		if !ok {
			return false
		}
		return value.Op == token.MUL || value.Op == token.NOT && kind == reusableScalarBool
	default:
		return false
	}
}

func canReuseValue(interp *Interp, value ssa.Value) bool {
	if interp.ctx.evalCallFn != nil || value == nil || !reusableValueProducerSupported(interp, value) {
		return false
	}
	referrers := value.Referrers()
	if referrers == nil || len(*referrers) == 0 {
		return false
	}
	kind, scalar := reusableScalarKindOf(value.Type())
	for _, referrer := range *referrers {
		if directCallConsumesValue(interp, referrer, value) {
			continue
		}
		if scalar && reusableScalarConsumerSupported(kind, referrer, value) {
			continue
		}
		return false
	}
	return true
}

func makeReusableScalarBinOp(pfn *function, interp *Interp, instr *ssa.BinOp) func(*frame) {
	kind, ok := reusableScalarKindOf(instr.X.Type())
	if !ok || !reusableScalarBinOpSupported(kind, instr.Op) {
		return nil
	}
	reuseResult := canReuseValue(interp, instr)
	if !reuseResult && !canReuseValue(interp, instr.X) && !canReuseValue(interp, instr.Y) {
		return nil
	}

	switch kind {
	case reusableScalarBool:
		return makeReusableBoolBinOp(pfn, instr, reuseResult)
	case reusableScalarInt:
		return makeReusableIntBinOp(pfn, instr, reuseResult)
	case reusableScalarFloat64:
		return makeReusableFloat64BinOp(pfn, instr, reuseResult)
	default:
		return nil
	}
}

func makeReusableScalarUnOp(pfn *function, interp *Interp, instr *ssa.UnOp) func(*frame) {
	reuseResult := canReuseValue(interp, instr)
	switch instr.Op {
	case token.NOT:
		kind, ok := reusableScalarKindOf(instr.X.Type())
		if !ok || kind != reusableScalarBool || !reuseResult && !canReuseValue(interp, instr.X) {
			return nil
		}
		ir := pfn.regIndex(instr)
		ix := pfn.regIndex(instr.X)
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, !fr.reusableBool(ix))
		}
	case token.MUL:
		if !reuseResult {
			return nil
		}
		return makeReusableScalarDeref(pfn, instr)
	default:
		return nil
	}
}

func makeReusableScalarIf(pfn *function, interp *Interp, instr *ssa.If) func(*frame) {
	if !canReuseValue(interp, instr.Cond) {
		return nil
	}
	kind, ok := reusableScalarKindOf(instr.Cond.Type())
	if !ok || kind != reusableScalarBool {
		return nil
	}
	ic := pfn.regIndex(instr.Cond)
	return func(fr *frame) {
		fr.pred = fr.block.Index
		if fr.reusableBool(ic) {
			fr.block = fr.block.Succs[0]
		} else {
			fr.block = fr.block.Succs[1]
		}
		fr.ipc = fr.pfn.Blocks[fr.block.Index]
	}
}

func makeReusableScalarDeref(pfn *function, instr *ssa.UnOp) func(*frame) {
	ir := pfn.regIndex(instr)
	ix, kx, vx := pfn.regIndex3(instr.X)
	kind, _ := reusableScalarKindOf(instr.Type())
	if kx == kindGlobal {
		ptr := reflect.ValueOf(vx)
		switch kind {
		case reusableScalarBool:
			return func(fr *frame) {
				elem := ptr.Elem()
				if !elem.IsValid() {
					panic(fr.runtimeError(instr, "invalid memory address or nil pointer dereference"))
				}
				setReusableValue(fr, ir, true, elem.Bool())
			}
		case reusableScalarInt:
			return func(fr *frame) {
				elem := ptr.Elem()
				if !elem.IsValid() {
					panic(fr.runtimeError(instr, "invalid memory address or nil pointer dereference"))
				}
				setReusableValue(fr, ir, true, int(elem.Int()))
			}
		case reusableScalarFloat64:
			return func(fr *frame) {
				elem := ptr.Elem()
				if !elem.IsValid() {
					panic(fr.runtimeError(instr, "invalid memory address or nil pointer dereference"))
				}
				setReusableValue(fr, ir, true, elem.Float())
			}
		default:
			return nil
		}
	}
	switch kind {
	case reusableScalarBool:
		return func(fr *frame) {
			elem := reflect.ValueOf(fr.reg(ix)).Elem()
			if !elem.IsValid() {
				panic(fr.runtimeError(instr, "invalid memory address or nil pointer dereference"))
			}
			setReusableValue(fr, ir, true, elem.Bool())
		}
	case reusableScalarInt:
		return func(fr *frame) {
			elem := reflect.ValueOf(fr.reg(ix)).Elem()
			if !elem.IsValid() {
				panic(fr.runtimeError(instr, "invalid memory address or nil pointer dereference"))
			}
			setReusableValue(fr, ir, true, int(elem.Int()))
		}
	case reusableScalarFloat64:
		return func(fr *frame) {
			elem := reflect.ValueOf(fr.reg(ix)).Elem()
			if !elem.IsValid() {
				panic(fr.runtimeError(instr, "invalid memory address or nil pointer dereference"))
			}
			setReusableValue(fr, ir, true, elem.Float())
		}
	default:
		return nil
	}
}

func (fr *frame) reusableBool(ir register) bool {
	v := fr.stack[ir]
	if boxed, ok := v.(*reusableValue[bool]); ok {
		return boxed.value
	}
	return v.(bool)
}

func (fr *frame) reusableInt(ir register) int {
	v := fr.stack[ir]
	if boxed, ok := v.(*reusableValue[int]); ok {
		return boxed.value
	}
	return v.(int)
}

func (fr *frame) reusableFloat64(ir register) float64 {
	v := fr.stack[ir]
	if boxed, ok := v.(*reusableValue[float64]); ok {
		return boxed.value
	}
	return v.(float64)
}

func reusableBoolOperand(fr *frame, ir register, constant bool, static bool) bool {
	if constant {
		return static
	}
	return fr.reusableBool(ir)
}

func makeReusableBoolBinOp(pfn *function, instr *ssa.BinOp, reuseResult bool) func(*frame) {
	ir := pfn.regIndex(instr)
	ix, kx, vx := pfn.regIndex3(instr.X)
	iy, ky, vy := pfn.regIndex3(instr.Y)
	xConstant, yConstant := kx == kindConst, ky == kindConst
	x, y := false, false
	if xConstant {
		x = vx.(bool)
	}
	if yConstant {
		y = vy.(bool)
	}
	switch instr.Op {
	case token.EQL:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableBoolOperand(fr, ix, xConstant, x) == reusableBoolOperand(fr, iy, yConstant, y))
		}
	case token.NEQ:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableBoolOperand(fr, ix, xConstant, x) != reusableBoolOperand(fr, iy, yConstant, y))
		}
	default:
		return nil
	}
}

func reusableIntOperand(fr *frame, ir register, constant bool, static int) int {
	if constant {
		return static
	}
	return fr.reusableInt(ir)
}

func makeReusableIntBinOp(pfn *function, instr *ssa.BinOp, reuseResult bool) func(*frame) {
	ir := pfn.regIndex(instr)
	ix, kx, vx := pfn.regIndex3(instr.X)
	iy, ky, vy := pfn.regIndex3(instr.Y)
	xConstant, yConstant := kx == kindConst, ky == kindConst
	x, y := 0, 0
	if xConstant {
		x = vx.(int)
	}
	if yConstant {
		y = vy.(int)
	}
	switch instr.Op {
	case token.ADD:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableIntOperand(fr, ix, xConstant, x)+reusableIntOperand(fr, iy, yConstant, y))
		}
	case token.SUB:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableIntOperand(fr, ix, xConstant, x)-reusableIntOperand(fr, iy, yConstant, y))
		}
	case token.MUL:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableIntOperand(fr, ix, xConstant, x)*reusableIntOperand(fr, iy, yConstant, y))
		}
	case token.QUO:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableIntOperand(fr, ix, xConstant, x)/reusableIntOperand(fr, iy, yConstant, y))
		}
	case token.REM:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableIntOperand(fr, ix, xConstant, x)%reusableIntOperand(fr, iy, yConstant, y))
		}
	case token.AND:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableIntOperand(fr, ix, xConstant, x)&reusableIntOperand(fr, iy, yConstant, y))
		}
	case token.OR:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableIntOperand(fr, ix, xConstant, x)|reusableIntOperand(fr, iy, yConstant, y))
		}
	case token.XOR:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableIntOperand(fr, ix, xConstant, x)^reusableIntOperand(fr, iy, yConstant, y))
		}
	case token.AND_NOT:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableIntOperand(fr, ix, xConstant, x)&^reusableIntOperand(fr, iy, yConstant, y))
		}
	case token.LSS:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableIntOperand(fr, ix, xConstant, x) < reusableIntOperand(fr, iy, yConstant, y))
		}
	case token.LEQ:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableIntOperand(fr, ix, xConstant, x) <= reusableIntOperand(fr, iy, yConstant, y))
		}
	case token.GTR:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableIntOperand(fr, ix, xConstant, x) > reusableIntOperand(fr, iy, yConstant, y))
		}
	case token.GEQ:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableIntOperand(fr, ix, xConstant, x) >= reusableIntOperand(fr, iy, yConstant, y))
		}
	case token.EQL:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableIntOperand(fr, ix, xConstant, x) == reusableIntOperand(fr, iy, yConstant, y))
		}
	case token.NEQ:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableIntOperand(fr, ix, xConstant, x) != reusableIntOperand(fr, iy, yConstant, y))
		}
	default:
		return nil
	}
}

func reusableFloat64Operand(fr *frame, ir register, constant bool, static float64) float64 {
	if constant {
		return static
	}
	return fr.reusableFloat64(ir)
}

func makeReusableFloat64BinOp(pfn *function, instr *ssa.BinOp, reuseResult bool) func(*frame) {
	ir := pfn.regIndex(instr)
	ix, kx, vx := pfn.regIndex3(instr.X)
	iy, ky, vy := pfn.regIndex3(instr.Y)
	xConstant, yConstant := kx == kindConst, ky == kindConst
	x, y := 0.0, 0.0
	if xConstant {
		x = vx.(float64)
	}
	if yConstant {
		y = vy.(float64)
	}
	switch instr.Op {
	case token.ADD:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableFloat64Operand(fr, ix, xConstant, x)+reusableFloat64Operand(fr, iy, yConstant, y))
		}
	case token.SUB:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableFloat64Operand(fr, ix, xConstant, x)-reusableFloat64Operand(fr, iy, yConstant, y))
		}
	case token.MUL:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableFloat64Operand(fr, ix, xConstant, x)*reusableFloat64Operand(fr, iy, yConstant, y))
		}
	case token.QUO:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableFloat64Operand(fr, ix, xConstant, x)/reusableFloat64Operand(fr, iy, yConstant, y))
		}
	case token.LSS:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableFloat64Operand(fr, ix, xConstant, x) < reusableFloat64Operand(fr, iy, yConstant, y))
		}
	case token.LEQ:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableFloat64Operand(fr, ix, xConstant, x) <= reusableFloat64Operand(fr, iy, yConstant, y))
		}
	case token.GTR:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableFloat64Operand(fr, ix, xConstant, x) > reusableFloat64Operand(fr, iy, yConstant, y))
		}
	case token.GEQ:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableFloat64Operand(fr, ix, xConstant, x) >= reusableFloat64Operand(fr, iy, yConstant, y))
		}
	case token.EQL:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableFloat64Operand(fr, ix, xConstant, x) == reusableFloat64Operand(fr, iy, yConstant, y))
		}
	case token.NEQ:
		return func(fr *frame) {
			setReusableValue(fr, ir, reuseResult, reusableFloat64Operand(fr, ix, xConstant, x) != reusableFloat64Operand(fr, iy, yConstant, y))
		}
	default:
		return nil
	}
}
