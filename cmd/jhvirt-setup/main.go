package main

import (
	"flag"
	"fmt"
	"github.com/Variel42k/ovirt-backup/internal/setup"
	"os"
)

func main() {
	operation := flag.String("setup", "", "операция установщика")
	flag.Parse()
	if err := setup.Run(append([]string{*operation}, flag.Args()...), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
