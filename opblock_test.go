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
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestNoopFunctionPreservesDebugRefs(t *testing.T) {
	seen := make(map[string]int)
	ctx := NewContext(0)
	ctx.SetDebug(func(info *DebugInfo) {
		variable, _, ok := info.AsVar()
		if ok {
			seen[variable.Name()]++
		}
	})
	// NewContext creates the SSA builder before SetDebug enables GlobalDebug.
	ctx.Builder.Reset()

	const source = `package main

func emptyFunction() {
	functionDebug := 1
	_ = functionDebug
}

func main() {
	emptyFunction()
	seed := 2
	func() {
		closureDebug := seed
		_ = closureDebug
	}()
}
`
	if _, err := ctx.RunFile("main.go", source, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"functionDebug", "closureDebug"} {
		if seen[name] == 0 {
			t.Errorf("debug variable %q was not observed; saw %v", name, seen)
		}
	}
}

func TestNoopFunctionPreservesTracing(t *testing.T) {
	var output bytes.Buffer
	previousOutput := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(&output)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(previousOutput)
		log.SetFlags(previousFlags)
	}()

	const source = `package main

func emptyFunction() {}

func main() {
	emptyFunction()
	func() {}()
}
`
	if _, err := NewContext(EnableTracing).RunFile("main.go", source, nil); err != nil {
		t.Fatal(err)
	}
	trace := output.String()
	for _, function := range []string{"main.emptyFunction", "main.main$1"} {
		if !strings.Contains(trace, "Entering "+function) {
			t.Errorf("trace did not enter %s:\n%s", function, trace)
		}
	}
}
