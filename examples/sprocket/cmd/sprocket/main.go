package main

import (
	"context"
	"fmt"
	"os"

	"github.com/crb2nu/sprocket/internal/server"
)

func main() {
	if err := server.Run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "sprocket: %v\n", err)
		os.Exit(1)
	}
}
