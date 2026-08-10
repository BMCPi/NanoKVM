package network

import (
	"testing"
	"time"
)

func TestGrowBackoff(t *testing.T) {
	if got := growBackoff(supRetryFloor); got != 2*supRetryFloor {
		t.Errorf("growBackoff(floor) = %s, want %s", got, 2*supRetryFloor)
	}
	if got := growBackoff(supRetryCap); got != supRetryCap {
		t.Errorf("growBackoff(cap) = %s, want %s (capped)", got, supRetryCap)
	}
	if got := growBackoff(45 * time.Second); got != supRetryCap {
		t.Errorf("growBackoff(45s) = %s, want cap %s", got, supRetryCap)
	}
}
