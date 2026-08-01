package main

import (
	"context"
	"log"
	"time"
)

// Noticing when the machine went to sleep.
//
// This daemon is usually run on a laptop, and a closed lid is by far the most
// common reason the phone stops receiving: the process is frozen, the
// WebSocket dies, and from the phone it looks exactly like the app having been
// killed. Those two have nothing in common except the symptom, and guessing
// between them costs a day of waiting each time.
//
// A suspended process cannot observe its own suspension while it is happening,
// but it can see the hole afterwards: a ticker that should fire every 30s
// firing 47 minutes late means 47 minutes passed with nothing running.

const suspendTick = 30 * time.Second

// isSuspendGap reports whether the delay between two ticks is too large to be
// ordinary scheduling. A loaded machine can be late by a good fraction of the
// interval, so the threshold is generous — the thing being detected is minutes
// or hours, and a false positive here would be worse than useless, since it
// would send someone looking for a suspend that never happened.
func isSuspendGap(gap, tick time.Duration) bool {
	return gap > tick*3
}

func watchForSuspend(ctx context.Context) {
	t := time.NewTicker(suspendTick)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			gap := now.Sub(last)
			last = now
			if !isSuspendGap(gap, suspendTick) {
				continue
			}
			log.Printf("wad: %s passed between ticks — this machine was asleep. "+
				"Nothing was delivered during it, and the phone's socket will have "+
				"dropped; it reconnects on its own once this is reachable again. "+
				"If that keeps happening, run the daemon somewhere that stays awake "+
				"(see wad/DEPLOY.md).", gap.Round(time.Second))
		}
	}
}
