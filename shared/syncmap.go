package shared

// Source: https://github.com/donomii/genericsyncmap/blob/228aa64aa12fc1e1101c2aca796f530ccba9e06b/syncmap.go

import (
	"iter"
	"sync"
)

// Map provides a thread-safe generic map built on top of sync.Map.
//
// The zero value is ready for use. A Map must not be copied after first use.
type Map[K comparable, V any] struct {
	m sync.Map
}

// SyncMap is an alias for Map retained for compatibility.
type SyncMap[K comparable, V any] = Map[K, V]

// NewSyncMap creates a new thread-safe generic map.
func NewSyncMap[K comparable, V any]() *SyncMap[K, V] {
	return &Map[K, V]{}
}

// Store sets a key-value pair.
func (sm *Map[K, V]) Store(key K, value V) {
	sm.m.Store(key, value)
}

// Load gets a value by key, returning the value and whether it was found.
func (sm *Map[K, V]) Load(key K) (V, bool) {
	var zero V
	if val, ok := sm.m.Load(key); ok {
		v, ok := val.(V)
		if !ok {
			return zero, false
		}
		return v, true
	}

	return zero, false
}

// LoadOrStore gets an existing value or stores a new one, returning the actual value and whether it was loaded.
func (sm *Map[K, V]) LoadOrStore(key K, value V) (V, bool) {
	actual, loaded := sm.m.LoadOrStore(key, value)

	v, ok := actual.(V)
	if !ok {
		var zero V
		return zero, loaded
	}

	return v, loaded
}

// LoadAndDelete gets an existing value and deletes it, returning the value and whether it was loaded.
func (sm *Map[K, V]) LoadAndDelete(key K) (V, bool) {
	var zero V
	val, loaded := sm.m.LoadAndDelete(key)
	if loaded {
		v, ok := val.(V)
		if !ok {
			return zero, false
		}
		return v, true
	}

	return zero, false
}

// Delete removes a key.
func (sm *Map[K, V]) Delete(key K) {
	sm.m.Delete(key)
}

// Clear deletes all entries.
func (sm *Map[K, V]) Clear() {
	sm.m.Clear()
}

// Swap swaps the value for a key and returns the previous value if any.
// The loaded result reports whether the key was present.
func (sm *Map[K, V]) Swap(key K, value V) (previous V, loaded bool) {
	var zero V
	prev, loaded := sm.m.Swap(key, value)
	if loaded {
		v, ok := prev.(V)
		if !ok {
			return zero, false
		}
		return v, true
	}

	return zero, false
}

// CompareAndSwap swaps the old and new values for key if the value stored in the map is equal to old.
// It panics if old is not comparable, matching sync.Map.
func (sm *Map[K, V]) CompareAndSwap(key K, o V, n V) (swapped bool) {
	return sm.m.CompareAndSwap(key, o, n)
}

// CompareAndDelete deletes the entry for key if its value is equal to old.
// It panics if old is not comparable, matching sync.Map.
func (sm *Map[K, V]) CompareAndDelete(key K, old V) (deleted bool) {
	return sm.m.CompareAndDelete(key, old)
}

// All returns an iterator over the keys and values in the map.
// All does not necessarily observe a consistent snapshot when the map is modified concurrently.
func (sm *Map[K, V]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		sm.m.Range(func(key, value any) bool {
			k, kOk := key.(K)
			v, vOk := value.(V)

			if !kOk || !vOk {
				var (
					zK K
					zV V
				)
				return yield(zK, zV)
			}

			return yield(k, v)
		})
	}
}

// Range calls fn sequentially for each key-value pair. Returning false stops the iteration.
// Range does not necessarily observe a consistent snapshot when the map is modified concurrently.
func (sm *Map[K, V]) Range(fn func(key K, value V) bool) {
	sm.All()(fn)
}

// Keys returns the keys encountered during one Range call.
// It does not return a consistent snapshot when the map is modified concurrently.
func (sm *Map[K, V]) Keys() []K {
	var keys []K
	sm.Range(func(key K, value V) bool {
		keys = append(keys, key)
		return true
	})
	return keys
}

// Values returns the values encountered during one Range call.
// It does not return a consistent snapshot when the map is modified concurrently.
func (sm *Map[K, V]) Values() []V {
	var values []V
	sm.Range(func(key K, value V) bool {
		values = append(values, value)
		return true
	})
	return values
}

// Len returns the number of keys encountered during one Range call in O(N) time.
// It does not return a consistent snapshot when the map is modified concurrently.
func (sm *Map[K, V]) Len() int {
	count := 0
	sm.Range(func(key K, value V) bool {
		count++
		return true
	})
	return count
}
