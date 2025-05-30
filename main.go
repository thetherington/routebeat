package main

import (
	"os"

	"github.com/thetherington/routebeat/cmd"

	_ "github.com/thetherington/routebeat/include"
)

func main() {
	if err := cmd.RootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
