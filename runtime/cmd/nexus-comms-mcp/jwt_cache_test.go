package main

import (
	"testing"
	"time"
)

// NEX-237: jwtCache returns the cached JWT until near-expiry so the
// happy path doesn't re-validate against the broker on every dial.
// The wsclient TokenProvider callback (registered in main.go) checks
// `time.Until(expires) > 1*time.Minute` before deciding to re-fetch
// — these tests pin Get/Set behave correctly under that contract.
func TestJWTCache_GetSetRoundTrip(t *testing.T) {
	c := &jwtCache{}
	if jwt, exp := c.Get(); jwt != "" || !exp.IsZero() {
		t.Errorf("fresh cache should return zero values; got jwt=%q exp=%v", jwt, exp)
	}
	want := "tok-abc"
	wantExp := time.Now().Add(12 * time.Hour)
	c.Set(want, wantExp)
	gotJWT, gotExp := c.Get()
	if gotJWT != want {
		t.Errorf("jwt = %q, want %q", gotJWT, want)
	}
	if !gotExp.Equal(wantExp) {
		t.Errorf("expires = %v, want %v", gotExp, wantExp)
	}
}

// NEX-237: Set replaces atomically — last write wins. Concurrent
// readers always see a coherent (jwt, expires) pair, never one from
// the previous write paired with one from the next.
func TestJWTCache_SetReplacesAtomically(t *testing.T) {
	c := &jwtCache{}
	c.Set("first", time.Unix(1000, 0))
	c.Set("second", time.Unix(2000, 0))
	jwt, exp := c.Get()
	if jwt != "second" || exp.Unix() != 2000 {
		t.Errorf("got (%q, %d), want (second, 2000)", jwt, exp.Unix())
	}
}

// NEX-237: concurrent Set + Get must not race. Pure smoke — go test
// -race catches the actual data race. Without the mutex on jwtCache
// this would flag.
func TestJWTCache_ConcurrentSafe(t *testing.T) {
	c := &jwtCache{}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			c.Set("w", time.Unix(int64(i), 0))
		}
		close(done)
	}()
	for i := 0; i < 1000; i++ {
		_, _ = c.Get()
	}
	<-done
}
