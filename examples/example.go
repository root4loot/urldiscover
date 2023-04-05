package main

import (
	"fmt"

	"github.com/root4loot/urldiscover/pkg/options"
	"github.com/root4loot/urldiscover/pkg/runner"
)

func main() {
	options := options.Options{
		Include:               []string{"example.com"},
		Exclude:               []string{"support.hackerone.com"},
		Concurrency:           20,
		Timeout:               10,
		ResponseHeaderTimeout: 10,
		Delay:                 0,
		DelayJitter:           0,
		UserAgent:             "urldiscover",
	}

	runner := runner.NewRunner(&options)

	// create a separate goroutine to process the results as they come in
	go func() {
		for result := range runner.Results {
			fmt.Println(result.StatusCode, result.RequestURL, result.Error)
		}
	}()

	// start the runner and begin processing results
	runner.Run("hackerone.com")
}
