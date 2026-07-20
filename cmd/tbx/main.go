package main

import (
	"fmt"
	"log"
	"os"

	"github.com/randax/talos-box/internal/version"
)

func main() {
	if len(os.Args) == 1 {
		printHelp(os.Stdout)
		return
	}

	switch os.Args[1] {
	case "version":
		fmt.Println(version.Version)
	case "help", "-h", "--help":
		printHelp(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "tbx: unknown command %q\n", os.Args[1])
		printHelp(os.Stderr)
		os.Exit(2)
	}
}

func printHelp(output *os.File) {
	const help = `Usage: tbx <command>

Commands:
  version  print the talosbox version
`
	if _, err := fmt.Fprint(output, help); err != nil {
		log.Printf("tbx: write help: %v", err)
	}
}
