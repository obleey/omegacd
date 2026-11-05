package main

import "testing"

func TestLocalHostDisabled(t *testing.T) {
	for v, want := range map[string]bool{"": false, "true": false, "TRUE": false, " false ": true, "false": true} {
		if got, err := localHostDisabled(v); err != nil || got != want {
			t.Errorf("%q: %v, %v", v, got, err)
		}
	}
	for _, v := range []string{"no", "0", "off", "yes"} {
		if _, err := localHostDisabled(v); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
}
