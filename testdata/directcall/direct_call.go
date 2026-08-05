package directcall

// Counter is a host type used by direct-call interpreter tests.
type Counter int

// Value returns the current counter value.
func (c *Counter) Value() int {
	return int(*c)
}

// FallbackCounter is a host type without a generated direct-call adapter.
type FallbackCounter int

// Value returns the current fallback counter value.
func (c *FallbackCounter) Value() int {
	return int(*c)
}

// ValueCounter verifies pointer calls to a value-receiver method.
type ValueCounter int

// Value returns the current counter value.
func (c ValueCounter) Value() int {
	return int(c)
}

// Recorder is a host type used to verify Go and defer interface invocation.
type Recorder struct {
	Values chan int
}

// Record publishes value to Values.
func (r *Recorder) Record(value int) {
	r.Values <- value
}

// Callback is a named callback type used by direct-call adapter tests.
type Callback func()

// Repeat calls callback count times.
func Repeat(count int, callback func()) {
	for range count {
		callback()
	}
}

// RepeatNamed calls a named callback count times.
func RepeatNamed(count int, callback Callback) {
	for range count {
		callback()
	}
}

// Retain returns callback so tests can invoke it after its adapter returns.
func Retain(callback func()) func() {
	return callback
}

// Check returns the result of condition.
func Check(condition func() bool) bool {
	return condition()
}

// CheckError returns the result of callback.
func CheckError(callback func() error) error {
	return callback()
}
