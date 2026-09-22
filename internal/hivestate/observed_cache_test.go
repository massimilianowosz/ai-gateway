package hivestate

import (
	"testing"
	"time"
)

func TestObservedCacheStore_RecordAndGet(t *testing.T) {
	s := newObservedCacheStore(time.Hour)
	if _, ok := s.get("scope-a"); ok {
		t.Fatal("expected no observation before any record")
	}
	s.record("scope-a", 16896)
	got, ok := s.get("scope-a")
	if !ok || got != 16896 {
		t.Fatalf("got (%d, %v), want (16896, true)", got, ok)
	}
	if _, ok := s.get("scope-b"); ok {
		t.Fatal("a different scope must not see another scope's observation")
	}
}

func TestObservedCacheStore_StaleEntryExpires(t *testing.T) {
	s := newObservedCacheStore(1 * time.Millisecond)
	s.record("scope-a", 16896)
	time.Sleep(5 * time.Millisecond)
	if _, ok := s.get("scope-a"); ok {
		t.Fatal("expected the entry to have expired past its TTL")
	}
}

func TestObservedCacheStore_EmptyKeyIsANoOp(t *testing.T) {
	s := newObservedCacheStore(time.Hour)
	s.record("", 16896)
	if _, ok := s.get(""); ok {
		t.Fatal("an empty scope key must never be stored or matched")
	}
}
