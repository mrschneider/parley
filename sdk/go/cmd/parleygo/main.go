// Command parleygo parses .parley files, assigns content-addressed ids,
// signs (deterministic test keys) and prints the canonical JSON.
//
// usage: parleygo <file.parley> [--sign] [--thread]
//
//	--thread  also prints the thread (the acts' state machine) of the
//	          first negotiation in the file (states + trace)
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"example.org/parley-go"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: parleygo <file.parley> [--sign]")
		os.Exit(1)
	}
	src, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	doc, err := parley.ParseText(string(src))
	if err != nil {
		fmt.Fprintln(os.Stderr, "syntax error:", err)
		os.Exit(1)
	}
	msgs := parley.Finalize(doc)
	sign := false
	thread := false
	for _, a := range os.Args[2:] {
		if a == "--sign" {
			sign = true
		}
		if a == "--thread" {
			thread = true
		}
	}
	if sign {
		for i, m := range msgs {
			msgs[i] = parley.Sign(m, m.From.ID, m.Tenant)
		}
	}
	if thread && len(msgs) > 0 {
		th, err := parley.NewThreadFrom(0, msgs)
		if err != nil {
			fmt.Fprintln(os.Stderr, "thread:", err)
			os.Exit(1)
		}
		tb, _ := json.MarshalIndent(th, "", "  ")
		fmt.Println("\n--- THREAD ---")
		fmt.Println(string(tb))
	}
	out := map[string]any{"capabilities": doc.Capabilities, "messages": msgs}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}
