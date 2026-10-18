package auth

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func BenchmarkVerifyHMAC(b *testing.B) {
	verifier := verifierFor(b, nil)
	token := signHS(b, hmacSecret, jwt.MapClaims{
		"role": "webuser",
		"sub":  "user_123",
		"exp":  time.Now().Add(time.Hour).Unix(),
	})
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		if _, err := verifier.Verify(ctx, token); err != nil {
			b.Fatal(err)
		}
	}
}
