package main

import (
        "fmt"
        "os"

        "github.com/kro-run/kro/pkg/performance/cmd"
)

func main() {
        rootCmd := cmd.NewRootCmd()
        if err := rootCmd.Execute(); err != nil {
                fmt.Println(err)
                os.Exit(1)
        }
}