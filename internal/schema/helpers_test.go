package schema

import (
	"context"
	"time"
)

// cancelAfter returns an already-expired context and its cancel function, used
// to force a reload failure without touching the database.
func cancelAfter() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	time.Sleep(time.Millisecond)
	return ctx, cancel
}
