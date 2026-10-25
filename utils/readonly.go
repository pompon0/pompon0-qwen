package utils

import (
	"errors"
	"reflect"
	"sync"
)

func ErrorAs[T error](err error) Option[T] {
	if target, ok := errors.AsType[T](err); ok {
		return Some(target)
	}
	return None[T]()
}

// ReadOnly marks a struct so TestEqual compares its private fields.
type ReadOnly struct{}

// isReadOnly reports whether t embeds ReadOnly.
func isReadOnly(t reflect.Type) bool {
	want := reflect.TypeFor[ReadOnly]()
	if t.Kind() != reflect.Struct {
		return false
	}
	for i := range t.NumField() {
		if f := t.Field(i); f.Anonymous || f.Type == want {
			return true
		}
	}
	return false
}

// Slice constructs a slice.
// It is a syntax sugar for `[]T{v...}`, which avoids
// spelling out T. Not very useful if you need to spell
// out T to construct the elements: in that case
// you might prefer the []T{{...},{...}} syntax instead.
func Slice[T any](v ...T) []T { return v }

// NoCompare may be added to structs which must not be used as
// map keys.
type NoCompare [0]func()

// Zero returns a zero value of type T.
func Zero[T any]() (zero T) { return }

// Alloc moves value to heap.
func Alloc[T any](v T) *T { return &v }

// NoCopy may be added to structs which must not be copied
// after the first use.
//
// See https://golang.org/issues/8005#issuecomment-190753527
// for details.
//
// Note that it must not be embedded, otherwise Lock and Unlock methods
// will be exported.
type NoCopy struct{}

// Lock implements sync.Locker.
func (*NoCopy) Lock() {}

// Unlock implements sync.Locker.
func (*NoCopy) Unlock() {}

var _ sync.Locker = (*NoCopy)(nil)
