// SPDX-License-Identifier: GPL-3.0-or-later

// Command runnerclient runs one job's calls through forgejo-runner's own
// plugin client (act/plugin), as the runner makes them, against a plugin's
// listen socket. The plugin's tests build it and run it by path: the
// runner's generated plugin protocol and the plugin's own register the same
// proto file, so the two can't share a process. It never ships.
//
//	runnerclient [-option-bytes n] [-label arg] [-timeout d] <socket>
//	    connect to the plugin at socket, with the runner's own defaults;
//	    Create a job on the label argument, with a backend option of n bytes
//	    when n is above 0; then Remove the job. Print "ok" when every call
//	    succeeds, else the failed call and its error, and exit 1.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"code.forgejo.org/forgejo/runner/v13/act/container"
	actplugin "code.forgejo.org/forgejo/runner/v13/act/plugin"
)

func main() { os.Exit(runMain()) }

// runMain parses the flags, makes the calls and returns the exit code.
func runMain() int {
	optionBytes := flag.Int("option-bytes", 0, "the size of a backend option Create carries")
	label := flag.String("label", "", "the job's label argument")
	timeout := flag.Duration("timeout", 2*time.Minute, "how long every call may take together")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: runnerclient [flags] <socket>")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	var options map[string]string
	if *optionBytes > 0 {
		options = map[string]string{"big": strings.Repeat("x", *optionBytes)}
	}
	if err := run(ctx, flag.Arg(0), options, *label); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("ok")
	return 0
}

// run makes the job's calls, each named in its error.
func run(ctx context.Context, socket string, options map[string]string, label string) error {
	c, err := actplugin.NewClient(ctx, "unix://"+socket)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = c.Close() }()
	env := c.NewEnvironment(&container.NewContainerInput{Name: "job", Stdout: io.Discard, Stderr: io.Discard}, options, label, 0)
	if err := env.Create(nil, nil)(ctx); err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if err := env.Remove()(ctx); err != nil {
		return fmt.Errorf("remove: %w", err)
	}
	return nil
}
