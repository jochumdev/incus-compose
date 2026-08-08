package shared

import (
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSyncMap_Basic(t *testing.T) {
	m := NewSyncMap[string, int]()

	// Store
	m.Store("one", 1)
	m.Store("two", 2)

	// Load
	if v, ok := m.Load("one"); !ok || v != 1 {
		t.Errorf("expected 1, true; got %v, %v", v, ok)
	}
	if _, ok := m.Load("three"); ok {
		t.Error("expected false for missing key")
	}

	// LoadOrStore
	v, loaded := m.LoadOrStore("three", 3)
	if loaded || v != 3 {
		t.Errorf("expected 3, false; got %v, %v", v, loaded)
	}
	v, loaded = m.LoadOrStore("one", 100)
	if !loaded || v != 1 {
		t.Errorf("expected 1, true; got %v, %v", v, loaded)
	}

	// Delete
	m.Delete("two")
	if _, ok := m.Load("two"); ok {
		t.Error("expected false after delete")
	}
}

func TestSyncMap_ZeroValue(t *testing.T) {
	var m SyncMap[string, int]
	m.Store("key", 10)

	if v, ok := m.Load("key"); !ok || v != 10 {
		t.Errorf("zero value: expected 10, true; got %v, %v", v, ok)
	}
}

func TestMap_CanonicalName(t *testing.T) {
	var m Map[string, int]
	m.Store("key", 10)

	if v, ok := m.Load("key"); !ok || v != 10 {
		t.Errorf("Map: expected 10, true; got %v, %v", v, ok)
	}
}

func TestSyncMap_Clear(t *testing.T) {
	m := NewSyncMap[string, int]()
	m.Store("one", 1)
	m.Store("two", 2)
	m.Clear()

	if l := m.Len(); l != 0 {
		t.Errorf("Clear: expected length 0, got %d", l)
	}

	m.Store("three", 3)
	if v, ok := m.Load("three"); !ok || v != 3 {
		t.Errorf("Clear: expected reusable map with 3, true; got %v, %v", v, ok)
	}
}

func TestSyncMap_All(t *testing.T) {
	var m Map[int, int]
	expected := map[int]int{1: 10, 2: 20, 3: 30}
	for key, value := range expected {
		m.Store(key, value)
	}

	actual := make(map[int]int)
	for key, value := range m.All() {
		actual[key] = value
	}

	if len(actual) != len(expected) {
		t.Errorf("All: expected %d items, got %d", len(expected), len(actual))
	}
	for key, expectedValue := range expected {
		if actualValue := actual[key]; actualValue != expectedValue {
			t.Errorf("All: key %d expected value %d, got %d", key, expectedValue, actualValue)
		}
	}
}

func TestSyncMap_NewFeatures(t *testing.T) {
	m := NewSyncMap[string, int]()

	// LoadAndDelete
	m.Store("key", 10)
	v, loaded := m.LoadAndDelete("key")
	if !loaded || v != 10 {
		t.Errorf("LoadAndDelete: expected 10, true; got %v, %v", v, loaded)
	}
	_, loaded = m.LoadAndDelete("key")
	if loaded {
		t.Error("LoadAndDelete: expected false on second call")
	}

	// CompareAndDelete
	m.Store("key", 20)
	if m.CompareAndDelete("key", 999) {
		t.Error("CompareAndDelete: expected false for wrong value")
	}
	if !m.CompareAndDelete("key", 20) {
		t.Error("CompareAndDelete: expected true for correct value")
	}
	if _, ok := m.Load("key"); ok {
		t.Error("CompareAndDelete: expected key to be deleted")
	}

	// Swap
	m.Store("key", 30)
	prev, loaded := m.Swap("key", 40)
	if !loaded || prev != 30 {
		t.Errorf("Swap: expected 30, true; got %v, %v", prev, loaded)
	}
	if v, _ := m.Load("key"); v != 40 {
		t.Errorf("Swap: expected new value 40; got %v", v)
	}

	// CompareAndSwap
	if m.CompareAndSwap("key", 999, 50) {
		t.Error("CompareAndSwap: expected false for wrong old value")
	}
	if !m.CompareAndSwap("key", 40, 50) {
		t.Error("CompareAndSwap: expected true for correct old value")
	}
	if v, _ := m.Load("key"); v != 50 {
		t.Errorf("CompareAndSwap: expected new value 50; got %v", v)
	}
}

func TestSyncMap_Range(t *testing.T) {
	m := NewSyncMap[int, int]()
	expected := map[int]int{1: 10, 2: 20, 3: 30}
	for k, v := range expected {
		m.Store(k, v)
	}

	count := 0
	m.Range(func(key int, value int) bool {
		if expected[key] != value {
			t.Errorf("Range: expected value %d for key %d, got %d", expected[key], key, value)
		}
		count++
		return true
	})

	if count != len(expected) {
		t.Errorf("Range: expected %d items, got %d", len(expected), count)
	}
}

func TestSyncMap_KeysValuesLen(t *testing.T) {
	m := NewSyncMap[string, int]()
	data := map[string]int{"a": 1, "b": 2, "c": 3}
	for k, v := range data {
		m.Store(k, v)
	}

	// Len
	if l := m.Len(); l != 3 {
		t.Errorf("Len: expected 3, got %d", l)
	}

	// Keys
	keys := m.Keys()
	if len(keys) != 3 {
		t.Errorf("Keys: expected 3 keys, got %d", len(keys))
	}
	// Verify keys content (ignoring order)
	keyMap := make(map[string]bool)
	for _, k := range keys {
		keyMap[k] = true
	}
	for k := range data {
		if !keyMap[k] {
			t.Errorf("Keys: missing key %s", k)
		}
	}

	// Values
	values := m.Values()
	if len(values) != 3 {
		t.Errorf("Values: expected 3 values, got %d", len(values))
	}
	// Verify values
	valMap := make(map[int]bool)
	for _, v := range values {
		valMap[v] = true
	}
	for _, v := range data {
		if !valMap[v] {
			t.Errorf("Values: missing value %d", v)
		}
	}
}

func TestSyncMap_Concurrency(t *testing.T) {
	m := NewSyncMap[int, int]()
	var wg sync.WaitGroup
	workers := 100
	ops := 1000

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(id int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(id)))
			for j := 0; j < ops; j++ {
				key := rng.Intn(100)
				val := rng.Intn(1000)

				switch j % 4 {
				case 0:
					m.Store(key, val)
				case 1:
					m.Load(key)
				case 2:
					m.LoadOrStore(key, val)
				default:
					m.Delete(key)
				}
			}
		}(i)
	}
	wg.Wait()
}

// ExampleMap_usage demonstrates basic usage of Map.
func ExampleMap_usage() {
	var m Map[string, int]

	m.Store("apple", 10)
	m.Store("banana", 20)

	if val, ok := m.Load("apple"); ok {
		fmt.Printf("apple: %d\n", val)
	}

	m.Delete("banana")

	// Output:
	// apple: 10
}

// ExampleMap_all demonstrates how to iterate over the map.
func ExampleMap_all() {
	var m Map[string, string]
	m.Store("LOC", "USA")
	m.Store("LANG", "EN")

	// Collect keys for consistent output order
	var keys []string
	for key := range m.All() {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, k := range keys {
		v, _ := m.Load(k)
		fmt.Printf("%s: %s\n", k, v)
	}

	// Output:
	// LANG: EN
	// LOC: USA
}

// Benchmarks

const benchmarkKeyCount = 1024

type benchmarkLockedMap struct {
	mu     sync.RWMutex
	values map[int]int
}

func newBenchmarkLockedMap() *benchmarkLockedMap {
	return &benchmarkLockedMap{values: make(map[int]int)}
}

func (m *benchmarkLockedMap) Load(key int) (int, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	value, loaded := m.values[key]
	return value, loaded
}

func (m *benchmarkLockedMap) Store(key int, value int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[key] = value
}

type benchmarkOperations struct {
	load  func(int) (int, bool)
	store func(int, int)
}

func benchmarkConcurrentMixed(b *testing.B, operations benchmarkOperations) {
	var workerID atomic.Int64
	b.RunParallel(func(pb *testing.PB) {
		rng := rand.New(rand.NewSource(workerID.Add(1)))
		for pb.Next() {
			key := rng.Intn(benchmarkKeyCount)
			if rng.Intn(2) == 0 {
				operations.load(key)
			} else {
				operations.store(key, key)
			}
		}
	})
}

func BenchmarkLoadHit(b *testing.B) {
	b.Run("genericsyncmap", func(b *testing.B) {
		var m Map[int, int]
		m.Store(1, 1)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			m.Load(1)
		}
	})

	b.Run("sync-map", func(b *testing.B) {
		var m sync.Map
		m.Store(1, 1)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			value, loaded := m.Load(1)
			if loaded {
				_ = value.(int)
			}
		}
	})

	b.Run("rwmutex-map", func(b *testing.B) {
		m := newBenchmarkLockedMap()
		m.Store(1, 1)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			m.Load(1)
		}
	})
}

func BenchmarkStoreExisting(b *testing.B) {
	b.Run("genericsyncmap", func(b *testing.B) {
		var m Map[int, int]
		m.Store(1, 1)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			m.Store(1, i)
		}
	})

	b.Run("sync-map", func(b *testing.B) {
		var m sync.Map
		m.Store(1, 1)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			m.Store(1, i)
		}
	})

	b.Run("rwmutex-map", func(b *testing.B) {
		m := newBenchmarkLockedMap()
		m.Store(1, 1)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			m.Store(1, i)
		}
	})
}

func BenchmarkConcurrentMixed(b *testing.B) {
	b.Run("genericsyncmap", func(b *testing.B) {
		var m Map[int, int]
		for key := 0; key < benchmarkKeyCount; key++ {
			m.Store(key, key)
		}
		b.ReportAllocs()
		b.ResetTimer()
		benchmarkConcurrentMixed(b, benchmarkOperations{load: m.Load, store: m.Store})
	})

	b.Run("sync-map", func(b *testing.B) {
		var m sync.Map
		for key := 0; key < benchmarkKeyCount; key++ {
			m.Store(key, key)
		}
		b.ReportAllocs()
		b.ResetTimer()
		benchmarkConcurrentMixed(b, benchmarkOperations{
			load: func(key int) (int, bool) {
				value, loaded := m.Load(key)
				if loaded {
					return value.(int), true
				}
				return 0, false
			},
			store: func(key int, value int) {
				m.Store(key, value)
			},
		})
	})

	b.Run("rwmutex-map", func(b *testing.B) {
		m := newBenchmarkLockedMap()
		for key := 0; key < benchmarkKeyCount; key++ {
			m.Store(key, key)
		}
		b.ReportAllocs()
		b.ResetTimer()
		benchmarkConcurrentMixed(b, benchmarkOperations{load: m.Load, store: m.Store})
	})
}
