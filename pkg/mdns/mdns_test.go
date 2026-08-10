package mdns

import (
	"reflect"
	"testing"
)

func TestLocalNames(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"licheervnano", []string{"licheervnano.local"}},
		{"NanoKVM", []string{"nanokvm.local"}},
		{"host.local", []string{"host.local"}},
		{"host.local.", []string{"host.local"}},
		{"  Spaced  ", []string{"spaced.local"}},
		{"UPPER.LOCAL", []string{"upper.local"}},
		{"", nil},
		{"   ", nil},
		{".", nil},
	}
	for _, tc := range cases {
		got := localNames(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("localNames(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
