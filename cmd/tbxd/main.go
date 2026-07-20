package main

import (
	"fmt"
	"log"
	"os"

	"github.com/randax/talos-box/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println(version.Version)
		return
	}

	log.SetFlags(0)
	log.Print("tbxd: not yet implemented")
}
