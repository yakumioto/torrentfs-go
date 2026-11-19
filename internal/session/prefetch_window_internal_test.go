package session

import "testing"

func TestCoordinatorWindowFallsBackToTheCompileTimeDefault(t *testing.T) {
	const configured int64 = 8 << 20
	for _, tt := range []struct {
		name    string
		session *Session
		want    int64
	}{
		{name: "configured", session: &Session{prefetchWindow: configured}, want: configured},
		{name: "nil session", session: nil, want: defaultPrefetchHigh},
		{name: "zero window", session: &Session{}, want: defaultPrefetchHigh},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := coordinatorWindow(tt.session); got != tt.want {
				t.Fatalf("coordinatorWindow = %d, want %d", got, tt.want)
			}
		})
	}
}
