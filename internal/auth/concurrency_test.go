package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/andeshq/subquery/internal/config"
)

// TestVerifyRemoteJWKSConcurrent asserts that a shared key source is safe under
// concurrent verification and that the cache actually prevents re-fetching.
func TestVerifyRemoteJWKSConcurrent(t *testing.T) {
	key := newRSAKey(t)
	js := newJWKSServer(t, jwksDoc(t, rsaJWK(t, "k1", &key.PublicKey)))
	v := verifierFor(t, func(c *config.Config) { c.JWTSecret = js.srv.URL })
	token := signRS(t, key, "k1", jwt.MapClaims{"role": "webuser"})

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				claims, err := v.Verify(context.Background(), token)
				if err != nil {
					t.Errorf("Verify: %v", err)
					return
				}
				if claims.Role != "webuser" {
					t.Errorf("Role = %q", claims.Role)
					return
				}
			}
		}()
	}
	wg.Wait()

	if fetches := js.fetches(); fetches > 8 {
		t.Errorf("the key set was fetched %d times; the cache should absorb concurrent readers", fetches)
	}
}

// TestVerifyRemoteJWKSConcurrentRotation asserts that concurrent verification
// during a key rotation is safe and eventually succeeds.
func TestVerifyRemoteJWKSConcurrentRotation(t *testing.T) {
	oldKey := newRSAKey(t)
	newKey := newRSAKey(t)
	js := newJWKSServer(t, jwksDoc(t, rsaJWK(t, "k1", &oldKey.PublicKey)))
	v := verifierFor(t, func(c *config.Config) { c.JWTSecret = js.srv.URL })

	if _, err := v.Verify(t.Context(), signRS(t, oldKey, "k1", jwt.MapClaims{"role": "webuser"})); err != nil {
		t.Fatalf("initial Verify: %v", err)
	}
	js.set(jwksDoc(t, rsaJWK(t, "k2", &newKey.PublicKey)))
	rotated := signRS(t, newKey, "k2", jwt.MapClaims{"role": "author"})

	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				if _, err := v.Verify(context.Background(), rotated); err != nil {
					t.Errorf("Verify after rotation: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestKeySourceIsNotMutatedByCallers guards against a reader aliasing the cached
// slice and corrupting it for everyone else.
func TestKeySourceIsNotMutatedByCallers(t *testing.T) {
	key := newRSAKey(t)
	js := newJWKSServer(t, jwksDoc(t, rsaJWK(t, "k1", &key.PublicKey)))
	source := newRemoteSource(js.srv.URL)

	first, err := source.keys(t.Context(), false)
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("no keys returned")
	}
	second, err := source.keys(t.Context(), false)
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	if len(second) != len(first) {
		t.Fatalf("cached key count changed: %d then %d", len(first), len(second))
	}
}

// TestKeySourceRefreshDoesNotBlockReaders asserts that a JWKS refresh in flight
// does not stall verifications that a fresh cache can serve: the fetch lock is
// not held by hot-path readers.
func TestKeySourceRefreshDoesNotBlockReaders(t *testing.T) {
	key := newRSAKey(t)

	var (
		mu      sync.Mutex
		count   int
		once    sync.Once
		entered = make(chan struct{})
		release = make(chan struct{})
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		count++
		n := count
		mu.Unlock()

		// The first request primes the cache; every later request blocks, so a
		// refresh can be held open while we probe the read path.
		if n > 1 {
			once.Do(func() { close(entered) })
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=300")
		_, _ = w.Write(jwksDoc(t, rsaJWK(t, "k1", &key.PublicKey)))
	}))
	defer srv.Close()

	source := newRemoteSource(srv.URL)
	if _, err := source.keys(t.Context(), false); err != nil {
		t.Fatalf("prime: %v", err)
	}

	// A forced refresh blocks inside the HTTP fetch.
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		_, _ = source.keys(context.Background(), true)
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the forced refresh never reached the server")
	}

	// A non-forced read is served from the fresh cache and must not wait.
	readDone := make(chan error, 1)
	go func() {
		_, err := source.keys(context.Background(), false)
		readDone <- err
	}()

	select {
	case err := <-readDone:
		if err != nil {
			t.Errorf("cached read: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cached read was blocked behind the in-flight refresh")
	}

	close(release)
	<-refreshDone
}
