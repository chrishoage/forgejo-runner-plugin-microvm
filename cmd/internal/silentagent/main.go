// SPDX-License-Identifier: GPL-3.0-or-later

// Command silentagent is an agent that never answers: it starts as libkrun's
// workload, as the agent does, and then only waits, listening on nothing,
// until the VM is killed. The KVM tests build it, static (CGO_ENABLED=0), and
// put it in the place of the agent the plugin extracted, so a VM boots
// whose agent never answers Ping. It never ships.
package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	fmt.Fprintln(os.Stderr, "silentagent: started; it never answers")
	// A sleeping goroutine, not an empty select, which the runtime would
	// end as a deadlock.
	for {
		time.Sleep(time.Hour)
	}
}
