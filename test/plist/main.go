// Command plist renders minitail's LaunchAgent property list to stdout, so
// that CI can hand it to `plutil -lint` on a real macOS runner.
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/mkmik/minitail/internal/launchagent"
)

func main() {
	program := "/opt/homebrew/bin/minitail"
	if len(os.Args) > 1 {
		program = os.Args[1]
	}
	spec, err := launchagent.DefaultSpec(program)
	if err != nil {
		log.Fatal(err)
	}
	data, err := spec.Plist()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(string(data))
}
