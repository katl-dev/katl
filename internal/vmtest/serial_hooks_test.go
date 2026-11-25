package vmtest

import (
	"context"
	"testing"
)

// A serial hook acts on complete lines only, so a chunk that ends in the middle
// of a signal line never fires it.
func TestSerialHookWaitsForCompleteLine(t *testing.T) {
	const line = "katlos-install progress: waiting for configuration at http://192.168.122.44:8080\n"
	serial := "booting\n" + line

	newHook := func() (*bool, []SerialHook) {
		fired := new(bool)
		return fired, []SerialHook{{
			Name:   "progress",
			Signal: "waiting for configuration at http://192.168.122.44:8080",
			Run: func(context.Context, SerialHookEvent) error {
				*fired = true
				return nil
			},
		}}
	}

	for split := 0; split < len(serial); split++ {
		fired, hooks := newHook()
		err := VMRunner{}.runSerialHooks(context.Background(), Result{}, VMConfig{SerialHooks: hooks}, VMPlan{}, serial[:split], make([]bool, 1))
		if err != nil {
			t.Fatalf("first %d bytes: %v", split, err)
		}
		if *fired {
			t.Fatalf("hook fired on the first %d of %d bytes", split, len(serial))
		}
	}

	fired, hooks := newHook()
	if err := (VMRunner{}).runSerialHooks(context.Background(), Result{}, VMConfig{SerialHooks: hooks}, VMPlan{}, serial, make([]bool, 1)); err != nil {
		t.Fatal(err)
	}
	if !*fired {
		t.Fatal("hook did not fire on the complete line")
	}
}
